package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
)

type APIKind int

const (
	APIResponses APIKind = iota + 1
	APIChatCompletions
)

type ImageInputError struct {
	Message string
}

func (e *ImageInputError) Error() string { return e.Message }

type imageOccurrence struct {
	part     map[string]any
	imageURL string
	focus    string
	index    int
	evidence Evidence
}

func TransformImages(ctx context.Context, root map[string]any, kind APIKind, resolver VisionResolver) (int, error) {
	occurrences, err := collectImages(root, kind)
	if err != nil {
		return 0, err
	}
	if len(occurrences) == 0 {
		return 0, nil
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var wg sync.WaitGroup
	var firstErr error
	var errOnce sync.Once
	for _, occurrence := range occurrences {
		occurrence := occurrence
		wg.Add(1)
		go func() {
			defer wg.Done()
			evidence, err := resolver.Analyze(ctx, occurrence.imageURL, occurrence.focus)
			if err != nil {
				errOnce.Do(func() {
					firstErr = fmt.Errorf("analyze image %d: %w", occurrence.index, err)
					cancel()
				})
				return
			}
			occurrence.evidence = evidence
		}()
	}
	wg.Wait()
	if firstErr != nil {
		return 0, firstErr
	}

	for _, occurrence := range occurrences {
		rendered, err := renderEvidence(occurrence.index, occurrence.evidence)
		if err != nil {
			return 0, fmt.Errorf("render image %d evidence: %w", occurrence.index, err)
		}
		for key := range occurrence.part {
			delete(occurrence.part, key)
		}
		if kind == APIResponses {
			occurrence.part["type"] = "input_text"
		} else {
			occurrence.part["type"] = "text"
		}
		occurrence.part["text"] = rendered
	}
	return len(occurrences), nil
}

func collectImages(root map[string]any, kind APIKind) ([]*imageOccurrence, error) {
	var field string
	if kind == APIResponses {
		field = "input"
	} else {
		field = "messages"
	}
	items, ok := root[field].([]any)
	if !ok {
		return nil, nil
	}

	var occurrences []*imageOccurrence
	for _, rawItem := range items {
		item, ok := rawItem.(map[string]any)
		if !ok {
			continue
		}
		for _, contentField := range []string{"content", "output"} {
			content, ok := item[contentField].([]any)
			if !ok {
				continue
			}
			focus := contentFocus(content)
			for _, rawPart := range content {
				part, ok := rawPart.(map[string]any)
				if !ok {
					continue
				}
				imageURL, isImage, err := imageURLFromPart(part)
				if err != nil {
					return nil, err
				}
				if !isImage {
					continue
				}
				occurrences = append(occurrences, &imageOccurrence{
					part:     part,
					imageURL: imageURL,
					focus:    focus,
					index:    len(occurrences) + 1,
				})
			}
		}
	}
	return occurrences, nil
}

func contentFocus(content []any) string {
	var texts []string
	for _, rawPart := range content {
		part, ok := rawPart.(map[string]any)
		if !ok {
			continue
		}
		partType, _ := part["type"].(string)
		if partType != "text" && partType != "input_text" && partType != "output_text" {
			continue
		}
		if text, ok := part["text"].(string); ok && strings.TrimSpace(text) != "" {
			texts = append(texts, text)
		}
	}
	return strings.TrimSpace(strings.Join(texts, "\n"))
}

func imageURLFromPart(part map[string]any) (string, bool, error) {
	partType, _ := part["type"].(string)
	switch partType {
	case "input_image":
		if imageURL, ok := stringImageURL(part["image_url"]); ok {
			return imageURL, true, nil
		}
		if _, hasFileID := part["file_id"]; hasFileID {
			return "", true, &ImageInputError{Message: "input_image.file_id is not supported because file IDs cannot be reused across providers"}
		}
		return "", true, &ImageInputError{Message: "input_image is missing image_url"}
	case "image_url":
		if imageURL, ok := stringImageURL(part["image_url"]); ok {
			return imageURL, true, nil
		}
		return "", true, &ImageInputError{Message: "image_url content is missing a URL"}
	case "image":
		data, dataOK := part["data"].(string)
		mime, mimeOK := part["mimeType"].(string)
		if !mimeOK {
			mime, mimeOK = part["mime_type"].(string)
		}
		if !dataOK || !mimeOK || !strings.HasPrefix(strings.ToLower(mime), "image/") {
			return "", true, &ImageInputError{Message: "image content requires base64 data and an image MIME type"}
		}
		return "data:" + mime + ";base64," + data, true, nil
	default:
		return "", false, nil
	}
}

func stringImageURL(value any) (string, bool) {
	switch image := value.(type) {
	case string:
		return image, strings.TrimSpace(image) != ""
	case map[string]any:
		urlValue, ok := image["url"].(string)
		return urlValue, ok && strings.TrimSpace(urlValue) != ""
	default:
		return "", false
	}
}

func decodeJSONObject(body []byte) (map[string]any, error) {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	var root map[string]any
	if err := decoder.Decode(&root); err != nil {
		return nil, err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return nil, errors.New("request contains multiple JSON values")
		}
		return nil, err
	}
	return root, nil
}
