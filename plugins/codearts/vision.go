package main

import "github.com/collegeming/cpa-jethub-plugins/internal/jethub/openai"

// imagePlaceholder replaces every image part on the way to CodeArts.
//
// It states that an image WAS dropped instead of discarding it silently: a model
// that never learns an image existed answers as if the user had typed nothing,
// which reads as a confident non-answer. DSH's own projection for text-only
// models spells the omission out for the same reason
// (`[image omitted because this model accepts text only; …]`,
// `dsh-llm/lib/index.js` projectImagesForTextModel).
const imagePlaceholder = "[image omitted: this CodeArts model accepts text only]"

// stripImagesForTextModel rewrites every image content part into a text
// placeholder and reports how many were replaced.
//
// EVERY model this plugin publishes is text-only, so the strip is
// unconditional. The chat endpoint refuses any request whose messages carry an
// image with
//
//	InferHub.001001020.406: The request model is not multimodal
//
// and the refusal covers the WHOLE request, not the last message: measured
// 2026-10-01 over 12 attempts per shape, text-only failed 0, an image in the
// final user message failed 6, and an image sitting only in the HISTORY failed
// 5. That last number is why this exists — the visible symptom is a plain-text
// turn failing for no apparent reason.
//
// Why project instead of reject: `DeepSeek-V4.1-Flash` is a POOLED name. Four
// upstreams publish it and only some read images (sensenova-max answered
// "Blue" for a solid-blue probe while codearts rejected the same request with
// 406). Rejecting here would take the whole pooled name down for any
// image-bearing turn, so the image degrades to text and the turn still gets an
// answer. This is the same trade the sibling adapters make for text-only
// models (`trae/translate.go` HasImageContent, atomcode `supports_vision`).
func stripImagesForTextModel(request *openai.Request) int {
	if request == nil {
		return 0
	}
	stripped := 0
	for position := range request.Messages {
		content, count := stripImagesFromContent(request.Messages[position].Content)
		if count == 0 {
			continue
		}
		request.Messages[position].Content = content
		stripped += count
	}
	return stripped
}

// stripImagesFromContent rewrites one message content value in place.
//
// Only the array form carries parts; a plain string has no image and is
// returned untouched, as is any value that is not a part list at all.
func stripImagesFromContent(content any) (any, int) {
	parts, okParts := content.([]any)
	if !okParts {
		return content, 0
	}
	out := make([]any, 0, len(parts))
	stripped := 0
	for _, raw := range parts {
		part, okPart := raw.(map[string]any)
		if !okPart {
			out = append(out, raw)
			continue
		}
		switch partType, _ := part["type"].(string); partType {
		case "image_url", "input_image", "image":
			out = append(out, map[string]any{"type": "text", "text": imagePlaceholder})
			stripped++
		case "tool-result", "tool_result":
			// A tool result nests its own content list; an image inside it
			// trips the same upstream check. Recurse rather than assume the
			// top level is the only place an image can hide.
			inner, count := stripImagesFromContent(part["content"])
			if count == 0 {
				out = append(out, raw)
				continue
			}
			copied := make(map[string]any, len(part))
			for key, value := range part {
				copied[key] = value
			}
			copied["content"] = inner
			out = append(out, copied)
			stripped += count
		default:
			out = append(out, raw)
		}
	}
	if stripped == 0 {
		// Returning the original slice (not a copy) keeps an image-free
		// request byte-identical to what the caller sent.
		return content, 0
	}
	return out, stripped
}
