// Package photoasset stores private, validated photo objects in Cloud Storage.
package photoasset

import (
	"context"
	"errors"
	"time"
)

const MaxBytes int64 = 5 * 1024 * 1024
const MaxLongEdge = 1920

var (
	ErrNotFound      = errors.New("photo object not found")
	ErrAlreadyExists = errors.New("photo object already exists")
	ErrInvalidImage  = errors.New("invalid photo image")
)

type Metadata struct {
	MIMEType      string
	SizeBytes     int64
	Width, Height int
	SHA256        string
}

type Object struct {
	Data        []byte
	Generation  int64
	ContentType string
}

type Storage interface {
	UploadURL(context.Context, string, Metadata, time.Time) (string, map[string]string, error)
	Read(context.Context, string, int64) (Object, error)
	Promote(context.Context, string, string, int64, Metadata) error
	Fence(context.Context, string) error
	Delete(context.Context, string) error
	ViewURL(context.Context, string, time.Time) (string, error)
}
