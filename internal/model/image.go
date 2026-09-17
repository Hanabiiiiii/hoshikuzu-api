package model

import "time"

type ImageType string

const (
	Desktop ImageType = "desktop"
	Mobile  ImageType = "mobile"
)

type Image struct {
	ID           int64     `json:"id"`
	UUID         string    `json:"uuid"`
	OriginalName string    `json:"original_name"`
	Filename     string    `json:"filename"`
	Type         ImageType `json:"type"`
	MIME         string    `json:"mime"`
	Extension    string    `json:"extension"`
	Size         int64     `json:"size"`
	Width        int       `json:"width"`
	Height       int       `json:"height"`
	CreatedAt    time.Time `json:"created_at"`
}
