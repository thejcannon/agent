//go:build ignore
// +build ignore

// Generates the mime type mappings.
package main

import (
	"bufio"
	"fmt"
	"net/http"
	"os"
	"path"
	"runtime"
	"strings"
	"text/template"
	"time"
)

var _, sourcePath, _, _ = runtime.Caller(0)
var targetFile = path.Join(path.Dir(sourcePath), "mime.go")
var urls = map[string]string{
	"nginx":  "https://hg.nginx.org/nginx/raw-file/default/conf/mime.types",
	"apache": "https://svn.apache.org/repos/asf/httpd/httpd/trunk/docs/conf/mime.types",
}

var mimeFileTemplate = template.Must(template.New("").Parse(
	`// Code generated by go generate; DO NOT EDIT.
// This file was auto-generated at
// {{ .Timestamp }}
// using data from the following sources:
// {{ .URLs.apache }}
// {{ .URLs.nginx }}
package mime

import (
	"mime"
)

// TypeByExtension returns a mime type for an extension.
func TypeByExtension(ext string) string {
	if mimeType, ok := types[ext]; ok {
		return mimeType
	}
	return mime.TypeByExtension(ext)
}

var types = map[string]string{
{{- range $key, $value := .Types }}
	".{{$key}}": "{{$value}}",
{{- end }}
}
`))

func writeMimeFile(file string, data interface{}) error {
	f, err := os.Create(file)
	if err != nil {
		return err
	}
	defer f.Close()
	return mimeFileTemplate.Execute(f, data)
}

func addApacheTypes(types map[string]string) error {
	client, err := http.Get(urls["apache"])
	if err != nil {
		return err
	}
	scanner := bufio.NewScanner(client.Body)
	// File format:
	// # comment
	// text/csv					csv
	// text/html					html htm
	for scanner.Scan() {
		line := strings.Trim(scanner.Text(), " \t")
		if len(line) == 0 || line[0:1] == "#" {
			continue
		}
		parts := strings.Fields(line)
		for _, ext := range parts[1:] {
			types[ext] = parts[0]
		}
	}
	client.Body.Close()
	return nil
}

func addNginxTypes(types map[string]string) error {
	client, err := http.Get(urls["nginx"])
	if err != nil {
		return err
	}
	var unfinishedLine string
	scanner := bufio.NewScanner(client.Body)
	// File format:
	// # comment
	// types {
	//   text/html html htm shtml;
	//   ...
	//   application/vnd.openxmlformats-officedocument.wordprocessingml.document
	//     docx;
	//   ...
	// }
	for scanner.Scan() {
		line := strings.Trim(scanner.Text(), " \t{}")
		if len(line) == 0 || line[0:1] == "#" || line == "types" {
			continue
		}
		if line[len(line)-1:] != ";" {
			unfinishedLine = line
			continue
		}
		if unfinishedLine != "" {
			line = unfinishedLine + " " + line
			unfinishedLine = ""
		}
		line = line[0 : len(line)-1]
		parts := strings.Fields(line)
		for _, ext := range parts[1:] {
			types[ext] = parts[0]
		}
	}
	client.Body.Close()
	return nil
}

func addYamlTypes(types map[string]string) error {
	// Most recent movement on the IEFT is
	// https://mailarchive.ietf.org/arch/msg/media-types/bdCyTe91zNz-i-9tuJGDa9bHcpQ/
	// where the thread collectively advocates text/yaml and discusses that
	// others are already using this
	const yamlMime = "text/yaml"
	if _, ok := types["yml"]; !ok {
		types["yml"] = yamlMime
	}
	if _, ok := types["yaml"]; !ok {
		types["yaml"] = yamlMime
	}
	return nil
}

func generate() error {
	types := make(map[string]string)
	err := addApacheTypes(types)
	if err != nil {
		return err
	}
	err = addNginxTypes(types)
	if err != nil {
		return err
	}
	err = addYamlTypes(types)
	if err != nil {
		return err
	}
	return writeMimeFile(targetFile, struct {
		Timestamp time.Time
		URLs      map[string]string
		Types     map[string]string
	}{
		Timestamp: time.Now(),
		URLs:      urls,
		Types:     types,
	})
}

func main() {
	err := generate()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
