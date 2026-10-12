package ui

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strings"

	tea "charm.land/bubbletea/v2"
	"github.com/atotto/clipboard"

	"git.estatecloud.org/radumaco/souvenir/model"
)

var imageTypes = []string{"image/png", "image/jpeg", "image/webp", "image/gif"}

const maxImageBytes = 10 << 20

type imagePastedMsg struct{ image model.Image }

type pasteFailedMsg struct{ err error }

func pasteFromClipboard() tea.Msg {
	img, err := clipboardImage()
	if err != nil {
		return pasteFailedMsg{err}
	}
	if img != nil {
		return imagePastedMsg{*img}
	}
	text, err := clipboard.ReadAll()
	if err != nil {
		return pasteFailedMsg{err}
	}
	return tea.PasteMsg{Content: text}
}

func clipboardImage() (*model.Image, error) {
	var list []string
	var read func(mediaType string) []string
	switch {
	case os.Getenv("WAYLAND_DISPLAY") != "" && installed("wl-paste"):
		list = []string{"wl-paste", "--list-types"}
		read = func(t string) []string { return []string{"wl-paste", "--type", t} }
	case os.Getenv("DISPLAY") != "" && installed("xclip"):
		list = []string{"xclip", "-selection", "clipboard", "-t", "TARGETS", "-o"}
		read = func(t string) []string { return []string{"xclip", "-selection", "clipboard", "-t", t, "-o"} }
	default:
		return nil, nil
	}
	out, err := exec.Command(list[0], list[1:]...).Output()
	if err != nil {
		return nil, nil
	}
	offered := strings.Fields(string(out))
	for _, t := range imageTypes {
		if !slices.Contains(offered, t) {
			continue
		}
		args := read(t)
		data, err := exec.Command(args[0], args[1:]...).Output()
		if err != nil {
			return nil, fmt.Errorf("reading the image: %w", err)
		}
		if len(data) > maxImageBytes {
			return nil, fmt.Errorf("the image is %.1f MB, the limit is %d MB", float64(len(data))/(1<<20), maxImageBytes>>20)
		}
		return &model.Image{MediaType: t, Data: bytes.Clone(data)}, nil
	}
	return nil, nil
}

func installed(name string) bool {
	_, err := exec.LookPath(name)
	return err == nil
}

func imageLabel(n int) string {
	if n == 1 {
		return "[image]"
	}
	return fmt.Sprintf("[%d images]", n)
}
