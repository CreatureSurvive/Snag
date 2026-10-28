package server

import (
	"archive/zip"
	"bytes"
)

func zipReader(data []byte) (*zip.Reader, error) {
	return zip.NewReader(bytes.NewReader(data), int64(len(data)))
}
