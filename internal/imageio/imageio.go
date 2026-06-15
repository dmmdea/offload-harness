// Package imageio loads a local image (or validates a data URI) into a
// data:image/...;base64,... URI for the multimodal vision path. It NEVER fetches
// a remote URL — only local files and pre-built data URIs are accepted.
package imageio

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"os"
	"strings"
)

// LoadImageB64 returns a data:image/<type>;base64,<...> URI for pathOrDataURI.
//
//   - If the arg is already a data: URI, it is validated to be an image/* base64
//     URI and returned unchanged (rejected otherwise).
//   - If it looks like an http(s) URL, it is rejected — remote fetches are never
//     allowed.
//   - Otherwise it is treated as a local file path: read (rejected if larger than
//     maxBytes), type-sniffed from magic bytes (PNG/JPEG/WebP/GIF), and
//     base64-encoded into a data URI. An unknown type is rejected.
//
// maxBytes <= 0 means "no size limit" (the file is read whatever its size).
func LoadImageB64(pathOrDataURI string, maxBytes int) (string, error) {
	if strings.HasPrefix(pathOrDataURI, "data:") {
		// data:image/<type>;base64,<...>
		rest := strings.TrimPrefix(pathOrDataURI, "data:")
		semi := strings.IndexByte(rest, ';')
		if semi < 0 {
			return "", fmt.Errorf("imageio: malformed data URI (no ';')")
		}
		mediaType := rest[:semi]
		if !strings.HasPrefix(mediaType, "image/") {
			return "", fmt.Errorf("imageio: data URI is %q, not image/*", mediaType)
		}
		marker := ";base64,"
		idx := strings.Index(rest[semi:], marker)
		if idx < 0 {
			return "", fmt.Errorf("imageio: data URI is not base64-encoded")
		}
		// Reject an empty payload (nothing after ";base64,"): it decodes to no
		// image and would only waste a model call.
		payloadStart := semi + idx + len(marker)
		if payloadStart >= len(rest) || rest[payloadStart:] == "" {
			return "", fmt.Errorf("imageio: data URI has an empty base64 payload")
		}
		return pathOrDataURI, nil
	}

	if hasURLScheme(pathOrDataURI) {
		return "", fmt.Errorf("imageio: remote URLs not allowed (%q)", pathOrDataURI)
	}

	data, err := os.ReadFile(pathOrDataURI)
	if err != nil {
		return "", fmt.Errorf("imageio: read %q: %w", pathOrDataURI, err)
	}
	if maxBytes > 0 && len(data) > maxBytes {
		return "", fmt.Errorf("imageio: image %d bytes exceeds limit %d", len(data), maxBytes)
	}
	typ, ok := sniffImageType(data)
	if !ok {
		return "", fmt.Errorf("imageio: %q is not a recognized image (png/jpeg/webp/gif)", pathOrDataURI)
	}
	b64 := base64.StdEncoding.EncodeToString(data)
	return "data:image/" + typ + ";base64," + b64, nil
}

// hasURLScheme reports whether s starts with an http(s) scheme.
func hasURLScheme(s string) bool {
	ls := strings.ToLower(s)
	return strings.HasPrefix(ls, "http://") || strings.HasPrefix(ls, "https://")
}

// sniffImageType returns the image subtype from magic bytes.
func sniffImageType(b []byte) (string, bool) {
	switch {
	case len(b) >= 8 && bytes.HasPrefix(b, []byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A}):
		return "png", true
	case len(b) >= 3 && bytes.HasPrefix(b, []byte{0xFF, 0xD8, 0xFF}):
		return "jpeg", true
	case len(b) >= 12 && bytes.HasPrefix(b, []byte("RIFF")) && bytes.Equal(b[8:12], []byte("WEBP")):
		return "webp", true
	case len(b) >= 4 && bytes.HasPrefix(b, []byte("GIF8")):
		return "gif", true
	}
	return "", false
}
