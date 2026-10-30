package runtime

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/r-hashi01/kairocode/internal/msg"
	"github.com/r-hashi01/kairocode/internal/store"
)

// Image is an image the user gives with a message (ADR 0026).
type Image struct {
	MediaType string
	Data      []byte
}

// ErrBadImage: an image is not taken (its kind, its size, how many).
var ErrBadImage = errors.New("image not taken")

// CheckImages refuses, before anything is stored, what the model would:
// a kind it does not read, more than MaxImageBytes, more than MaxImages.
// The bytes must be of the kind said. Errors name no content.
func CheckImages(images []Image) error {
	if len(images) > msg.MaxImages {
		return fmt.Errorf("%w: %d images, at most %d in a message", ErrBadImage, len(images), msg.MaxImages)
	}
	total := 0
	for _, im := range images {
		total += len(im.Data)
	}
	if total > msg.MaxRequestImageBytes {
		return fmt.Errorf("%w: the images are %d bytes together, at most %d", ErrBadImage, total, msg.MaxRequestImageBytes)
	}
	for i, im := range images {
		if !slices.Contains(msg.ImageTypes, im.MediaType) {
			return fmt.Errorf("%w: image %d is %q; %s are read", ErrBadImage, i+1, im.MediaType, strings.Join(msg.ImageTypes, ", "))
		}
		if len(im.Data) == 0 {
			return fmt.Errorf("%w: image %d is empty", ErrBadImage, i+1)
		}
		if len(im.Data) > msg.MaxImageBytes {
			return fmt.Errorf("%w: image %d is %d bytes, at most %d", ErrBadImage, i+1, len(im.Data), msg.MaxImageBytes)
		}
		if got := http.DetectContentType(im.Data); got != im.MediaType {
			return fmt.Errorf("%w: image %d is said to be %s, but is %s", ErrBadImage, i+1, im.MediaType, got)
		}
	}
	return nil
}

// SendImages is Send with images: one user message, the text and then the
// images, each stored once by its hash.
func (r *Runtime) SendImages(ctx context.Context, session, text string, images []Image) (int, error) {
	if err := CheckImages(images); err != nil {
		return 0, err
	}
	parts := []msg.Part{{Type: msg.Text, Text: text}}
	for _, im := range images {
		ref, err := r.Store.Put(ctx, store.KindImage, im.Data)
		if err != nil {
			return 0, err
		}
		parts = append(parts, msg.Part{Type: msg.Image, MediaType: im.MediaType, Ref: ref})
	}
	h, err := r.Store.PutMessage(ctx, msg.Message{Role: msg.User, Parts: parts})
	if err != nil {
		return 0, err
	}
	return r.sendMessage(ctx, session, h)
}

// ImageData is the bytes of the image stored as ref, for a client that
// shows a conversation again (ADR 0026).
func (r *Runtime) ImageData(ctx context.Context, ref string) ([]byte, error) {
	kind, data, err := r.Store.Get(ctx, ref)
	if err != nil {
		return nil, err
	}
	if kind != store.KindImage {
		return nil, fmt.Errorf("%w: %s", store.ErrNotFound, ref)
	}
	return data, nil
}

// withImages fills in the bytes of the images in ms, for a provider: the
// latest first, as many as one call takes (MaxRequestImages,
// MaxRequestImageBytes). An older one past that is said in words, not sent
// again: else every later call of the session would fail as too large.
func (r *Runtime) withImages(ctx context.Context, ms []msg.Message) ([]msg.Message, error) {
	count, total := 0, 0
	for i := len(ms) - 1; i >= 0; i-- {
		m := ms[i]
		for j := len(m.Parts) - 1; j >= 0; j-- {
			p := m.Parts[j]
			if p.Type != msg.Image {
				continue
			}
			kind, data, err := r.Store.Get(ctx, p.Ref)
			if err != nil {
				return nil, err
			}
			if kind != store.KindImage {
				return nil, fmt.Errorf("image %s: stored as %s", p.Ref, kind)
			}
			parts := slices.Clone(m.Parts)
			if count < msg.MaxRequestImages && total+len(data) <= msg.MaxRequestImageBytes {
				count, total = count+1, total+len(data)
				parts[j].Data = data
			} else {
				parts[j] = msg.Part{Type: msg.Text, Text: fmt.Sprintf("[an image (%s) given earlier, not sent again]", p.MediaType)}
			}
			m.Parts = parts
			ms[i] = m
		}
	}
	return ms, nil
}

// imageFile is where an image is given as a file, to an engine that takes
// none otherwise: images/<hash>.<ext> in the data directory. Named by its
// content, it is written the same however often.
func (r *Runtime) imageFile(ctx context.Context, p msg.Part) (string, error) {
	_, data, err := r.Store.Get(ctx, p.Ref)
	if err != nil {
		return "", err
	}
	name := strings.TrimPrefix(p.Ref, "sha256:")
	if _, err := hex.DecodeString(name); err != nil {
		return "", fmt.Errorf("image ref %q", p.Ref)
	}
	dir := filepath.Join(filepath.Dir(r.cfg.DB), "images")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	path := filepath.Join(dir, name+"."+strings.TrimPrefix(p.MediaType, "image/"))
	if _, err := os.Stat(path); err == nil {
		return path, nil
	}
	tmp, err := os.CreateTemp(dir, ".image-*") // one each: two turns may write the same image
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	return path, os.Rename(tmp.Name(), path)
}

// imageNote is the end of a prompt for an engine given images as files.
func (r *Runtime) imageNote(ctx context.Context, m msg.Message) (string, error) {
	var paths []string
	for _, p := range m.Parts {
		if p.Type != msg.Image {
			continue
		}
		path, err := r.imageFile(ctx, p)
		if err != nil {
			return "", err
		}
		paths = append(paths, path)
	}
	if len(paths) == 0 {
		return "", nil
	}
	return "\n\nAttached images (files):\n" + strings.Join(paths, "\n"), nil
}
