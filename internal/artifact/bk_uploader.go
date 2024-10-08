package artifact

import (
	"bytes"
	"cmp"
	"context"
	_ "crypto/sha512" // import sha512 to make sha512 ssl certs work
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptrace"
	"net/http/httputil"
	"net/url"
	"os"
	"slices"
	"strings"

	"github.com/buildkite/agent/v3/api"
	"github.com/buildkite/agent/v3/logger"
	"github.com/buildkite/agent/v3/version"
	"github.com/dustin/go-humanize"
)

const artifactPathVariable = "${artifact:path}"

const (
	// BKUploader uploads to S3 either as:
	// - a single signed POST, which has a hard limit of 5GB, or
	// - as a signed multipart, which has a limit of 5GB per _part_, but we
	//   aren't supporting larger artifacts yet.
	maxFormUploadedArtifactSize = int64(5 * 1024 * 1024 * 1024)

	// Multipart parts have a minimum size of 5MB.
	minPartSize = int64(5 * 1024 * 1024)
)

type BKUploaderConfig struct {
	// Whether or not HTTP calls should be debugged
	DebugHTTP bool
}

// BKUploader uploads artifacts to Buildkite itself.
type BKUploader struct {
	// The configuration
	conf BKUploaderConfig

	// The logger instance to use
	logger logger.Logger
}

// NewBKUploader creates a new Buildkite uploader.
func NewBKUploader(l logger.Logger, c BKUploaderConfig) *BKUploader {
	return &BKUploader{
		logger: l,
		conf:   c,
	}
}

// URL returns the empty string. BKUploader doesn't know the URL in advance,
// it is provided by Buildkite after uploading.
func (u *BKUploader) URL(*api.Artifact) string { return "" }

// CreateWork checks the artifact size, then creates one worker.
func (u *BKUploader) CreateWork(artifact *api.Artifact) ([]workUnit, error) {
	if artifact.FileSize > maxFormUploadedArtifactSize {
		return nil, errArtifactTooLarge{Size: artifact.FileSize}
	}
	actions := artifact.UploadInstructions.Actions
	if len(actions) == 0 {
		// Not multiple actions - use a single form upload.
		return []workUnit{&bkFormUpload{
			BKUploader: u,
			artifact:   artifact,
		}}, nil
	}

	// Ensure the actions are sorted by part number.
	slices.SortFunc(actions, func(a, b api.ArtifactUploadAction) int {
		return cmp.Compare(a.PartNumber, b.PartNumber)
	})

	// Split the artifact across multiple parts.
	chunks := int64(len(actions))
	chunkSize := artifact.FileSize / chunks
	remainder := artifact.FileSize % chunks
	var offset int64
	workUnits := make([]workUnit, 0, chunks)
	for i, action := range actions {
		size := chunkSize
		if int64(i) < remainder {
			// Spread the remainder across the first chunks.
			size++
		}
		workUnits = append(workUnits, &bkMultipartUpload{
			BKUploader: u,
			artifact:   artifact,
			partCount:  int(chunks),
			action:     &action,
			offset:     offset,
			size:       size,
		})
		offset += size
	}
	// After that loop, `offset` should equal `artifact.FileSize`.
	return workUnits, nil
}

// bkMultipartUpload uploads a single part of a multipart upload.
type bkMultipartUpload struct {
	*BKUploader
	artifact     *api.Artifact
	action       *api.ArtifactUploadAction
	partCount    int
	offset, size int64
}

func (u *bkMultipartUpload) Artifact() *api.Artifact { return u.artifact }

func (u *bkMultipartUpload) Description() string {
	return fmt.Sprintf("%s %s part %d/%d (~%s starting at ~%s)",
		u.artifact.ID,
		u.artifact.Path,
		u.action.PartNumber,
		u.partCount,
		humanize.IBytes(uint64(u.size)),
		humanize.IBytes(uint64(u.offset)),
	)
}

func (u *bkMultipartUpload) DoWork(ctx context.Context) (*api.ArtifactPartETag, error) {
	f, err := os.Open(u.artifact.AbsolutePath)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	f.Seek(u.offset, 0)
	lr := io.LimitReader(f, u.size)

	req, err := http.NewRequestWithContext(ctx, u.action.Method, u.action.URL, lr)
	if err != nil {
		return nil, err
	}
	// Content-Ranges are 0-indexed and inclusive
	// example: Content-Range: bytes 200-1000/67589
	req.Header.Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", u.offset, u.offset+u.size-1, u.artifact.FileSize))
	req.Header.Set("Content-Type", u.artifact.ContentType)
	req.Header.Add("User-Agent", version.UserAgent())

	if u.conf.DebugHTTP {
		dumpReqOut, err := httputil.DumpRequestOut(req, false)
		if err != nil {
			u.logger.Error("Couldn't dump outgoing request: %v", err)
		}
		u.logger.Debug("%s", dumpReqOut)
	}

	// TODO: set all the usual http transport & client options...
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if u.conf.DebugHTTP {
		dumpResp, err := httputil.DumpResponse(resp, true)
		if err != nil {
			u.logger.Error("Couldn't dump outgoing request: %v", err)
			return nil, err
		}
		u.logger.Debug("%s", dumpResp)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response body: %v", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, fmt.Errorf("unsuccessful status %s: %s", resp.Status, body)
	}

	etag := resp.Header.Get("Etag")
	u.logger.Debug("Artifact %s part %d has ETag = %s", u.artifact.ID, u.action.PartNumber, etag)
	if etag == "" {
		return nil, errors.New("response missing ETag header")
	}

	return &api.ArtifactPartETag{
		PartNumber: u.action.PartNumber,
		ETag:       etag,
	}, nil
}

// bkFormUpload uploads an artifact to a presigned URL in a single request using
// a request body encoded as multipart/form-data.
type bkFormUpload struct {
	*BKUploader
	artifact *api.Artifact
}

func (u *bkFormUpload) Artifact() *api.Artifact { return u.artifact }

func (u *bkFormUpload) Description() string {
	return singleUnitDescription(u.artifact)
}

// DoWork tries the upload.
func (u *bkFormUpload) DoWork(ctx context.Context) (*api.ArtifactPartETag, error) {
	request, err := createFormUploadRequest(ctx, u.logger, u.artifact)
	if err != nil {
		return nil, err
	}

	if u.conf.DebugHTTP {
		// If the request is a multi-part form, then it's probably a
		// file upload, in which case we don't want to spewing out the
		// file contents into the debug log (especially if it's been
		// gzipped)
		var requestDump []byte
		if strings.Contains(request.Header.Get("Content-Type"), "multipart/form-data") {
			requestDump, err = httputil.DumpRequestOut(request, false)
		} else {
			requestDump, err = httputil.DumpRequestOut(request, true)
		}

		if err != nil {
			u.logger.Debug("\nERR: %s\n%s", err, string(requestDump))
		} else {
			u.logger.Debug("\n%s", string(requestDump))
		}

		// configure the HTTP request to log the server IP. The IPs for s3.amazonaws.com
		// rotate every 5 seconds, and if one of them is misbehaving it may be helpful to
		// know which one.
		trace := &httptrace.ClientTrace{
			GotConn: func(connInfo httptrace.GotConnInfo) {
				u.logger.Debug("artifact %s uploading to: %s", u.artifact.ID, connInfo.Conn.RemoteAddr())
			},
		}
		request = request.WithContext(httptrace.WithClientTrace(request.Context(), trace))
	}

	// Create the client
	// TODO: this uses the default transport, potentially ignoring many agent
	// config options
	client := &http.Client{}

	// Perform the request
	u.logger.Debug("%s %s", request.Method, request.URL)
	response, err := client.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()

	if u.conf.DebugHTTP {
		responseDump, err := httputil.DumpResponse(response, true)
		if err != nil {
			u.logger.Debug("\nERR: %s\n%s", err, string(responseDump))
		} else {
			u.logger.Debug("\n%s", string(responseDump))
		}
	}

	if response.StatusCode/100 != 2 {
		body := &bytes.Buffer{}
		_, err := body.ReadFrom(response.Body)
		if err != nil {
			return nil, err
		}

		return nil, fmt.Errorf("%s (%d)", body, response.StatusCode)
	}
	return nil, nil
}

// Creates a new file upload http request with optional extra params
func createFormUploadRequest(ctx context.Context, _ logger.Logger, artifact *api.Artifact) (*http.Request, error) {
	streamer := newMultipartStreamer()
	action := artifact.UploadInstructions.Action

	// Set the post data for the request
	for key, val := range artifact.UploadInstructions.Data {
		// Replace the magical ${artifact:path} variable with the
		// artifact's path
		newVal := strings.ReplaceAll(val, artifactPathVariable, artifact.Path)

		// Write the new value to the form
		if err := streamer.WriteField(key, newVal); err != nil {
			return nil, err
		}
	}

	fh, err := os.Open(artifact.AbsolutePath)
	if err != nil {
		return nil, err
	}

	// It's important that we add the form field last because when
	// uploading to an S3 form, they are really nit-picky about the field
	// order, and the file needs to be the last one other it doesn't work.
	if err := streamer.WriteFile(action.FileInput, artifact.Path, fh); err != nil {
		fh.Close()
		return nil, err
	}

	// Create the URL that we'll send data to
	uri, err := url.Parse(action.URL)
	if err != nil {
		fh.Close()
		return nil, err
	}

	uri.Path = artifact.UploadInstructions.Action.Path

	// Create the request
	req, err := http.NewRequestWithContext(ctx, action.Method, uri.String(), streamer.Reader())
	if err != nil {
		fh.Close()
		return nil, err
	}

	// Setup the content type and length that s3 requires
	req.Header.Add("Content-Type", streamer.ContentType)
	// Letting the server know the agent version can be helpful for debugging
	req.Header.Add("User-Agent", version.UserAgent())
	req.ContentLength = streamer.Len()

	return req, nil
}

// A wrapper around the complexities of streaming a multipart file and fields to
// an http endpoint that infuriatingly requires a Content-Length
// Derived from https://github.com/technoweenie/multipartstreamer
type multipartStreamer struct {
	ContentType   string
	bodyBuffer    *bytes.Buffer
	bodyWriter    *multipart.Writer
	closeBuffer   *bytes.Buffer
	reader        io.ReadCloser
	contentLength int64
}

// newMultipartStreamer initializes a new MultipartStreamer.
func newMultipartStreamer() *multipartStreamer {
	m := &multipartStreamer{
		bodyBuffer: new(bytes.Buffer),
	}

	m.bodyWriter = multipart.NewWriter(m.bodyBuffer)
	boundary := m.bodyWriter.Boundary()
	m.ContentType = "multipart/form-data; boundary=" + boundary

	closeBoundary := fmt.Sprintf("\r\n--%s--\r\n", boundary)
	m.closeBuffer = bytes.NewBufferString(closeBoundary)

	return m
}

// WriteField writes a form field to the multipart.Writer.
func (m *multipartStreamer) WriteField(key, value string) error {
	return m.bodyWriter.WriteField(key, value)
}

// WriteFile writes the multi-part preamble which will be followed by file data
// This can only be called once and must be the last thing written to the streamer
func (m *multipartStreamer) WriteFile(key, artifactPath string, fh http.File) error {
	if m.reader != nil {
		return errors.New("WriteFile can't be called multiple times")
	}

	// Set up a reader that combines the body, the file and the closer in a stream
	m.reader = &multipartReadCloser{
		Reader: io.MultiReader(m.bodyBuffer, fh, m.closeBuffer),
		fh:     fh,
	}

	stat, err := fh.Stat()
	if err != nil {
		return err
	}

	m.contentLength = stat.Size()

	_, err = m.bodyWriter.CreateFormFile(key, artifactPath)
	return err
}

// Len calculates the byte size of the multipart content.
func (m *multipartStreamer) Len() int64 {
	return m.contentLength + int64(m.bodyBuffer.Len()) + int64(m.closeBuffer.Len())
}

// Reader gets an io.ReadCloser for passing to an http.Request.
func (m *multipartStreamer) Reader() io.ReadCloser {
	return m.reader
}

type multipartReadCloser struct {
	io.Reader
	fh http.File
}

func (mrc *multipartReadCloser) Close() error {
	return mrc.fh.Close()
}

type errArtifactTooLarge struct {
	Size int64
}

func (e errArtifactTooLarge) Error() string {
	// TODO: Clean up error strings
	// https://github.com/golang/go/wiki/CodeReviewComments#error-strings
	return fmt.Sprintf("File size (%d bytes) exceeds the maximum supported by Buildkite's default artifact storage (5Gb). Alternative artifact storage options may support larger files.", e.Size)
}