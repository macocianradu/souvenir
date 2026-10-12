package webapp

import (
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"slices"

	"git.estatecloud.org/radumaco/souvenir/model"
)

const (
	maxImages     = 4
	maxImageBytes = 10 << 20
	// maxUpload bounds a whole message: its text and its images.
	maxUpload = maxImages*maxImageBytes + 1<<20
)

// formImages reads the images pasted into a message. Their type is decided
// from their bytes, not from what the browser claims.
func formImages(r *http.Request) ([]model.Image, error) {
	if r.MultipartForm == nil {
		return nil, nil
	}
	files := r.MultipartForm.File["images"]
	if len(files) > maxImages {
		return nil, fmt.Errorf("A message can carry at most %d images.", maxImages)
	}
	var images []model.Image
	for _, fh := range files {
		img, err := readImage(fh)
		if err != nil {
			return nil, err
		}
		images = append(images, img)
	}
	return images, nil
}

func readImage(fh *multipart.FileHeader) (model.Image, error) {
	if fh.Size > maxImageBytes {
		return model.Image{}, fmt.Errorf("An image is %.1f MB; the limit is %d MB.", float64(fh.Size)/(1<<20), maxImageBytes>>20)
	}
	f, err := fh.Open()
	if err != nil {
		return model.Image{}, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxImageBytes+1))
	if err != nil {
		return model.Image{}, err
	}
	if len(data) > maxImageBytes {
		return model.Image{}, fmt.Errorf("An image is over the %d MB limit.", maxImageBytes>>20)
	}
	mediaType := http.DetectContentType(data)
	if !slices.Contains(imageTypes, mediaType) {
		return model.Image{}, errors.New("Only PNG, JPEG, WebP and GIF images can be attached.")
	}
	return model.Image{MediaType: mediaType, Data: data}, nil
}
