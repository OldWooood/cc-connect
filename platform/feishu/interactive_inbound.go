package feishu

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"github.com/chenhg5/cc-connect/core"

	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
)

// This file implements inbound interactive (card) message support.
//
// Background: when a user sends or forwards a card message to the bot,
// Feishu delivers it via im.message.receive_v1 with message_type
// "interactive". The event body does NOT carry the raw card JSON that was
// used at send time; instead it carries a simplified receive format:
//
//	{"title": "...", "elements": [[{"tag":"text",...},{"tag":"at",...}], ...]}
//
// Element tags observed in the wild (see "Receive message content structure"
// in the Feishu open docs): text, a, at, button, img, hr, note,
// select_static, overflow, date_picker.
//
// The entry points below translate that structure into the same
// core.Message shape the "text"/"post" branches produce, so @bot mentions,
// quoted replies, group history, and merge_forward all work for cards:
//
//   - extractInteractiveReceiveParts — pure parser, no network.
//   - extractInteractiveReceiveText — pure text renderer (history + quotes).
//   - parseInteractiveContent — parser + image download (live dispatch).
//   - downloadInteractiveImages — best-effort image fetch, failures skipped.

// interactiveReceiveElement is one cell of the simplified receive-format
// card body. Only the fields the inbound path needs are modelled; unknown
// fields are ignored so future tags degrade gracefully instead of failing.
type interactiveReceiveElement struct {
	Tag         string          `json:"tag"`
	Text        string          `json:"text"`
	Href        string          `json:"href"`
	UserID      string          `json:"user_id"`
	UserName    string          `json:"user_name"`
	ImageKey    string          `json:"image_key"`
	Placeholder string          `json:"placeholder"`
	InitialDate string          `json:"initial_date"`
	Options     json.RawMessage `json:"options"`
	Elements    json.RawMessage `json:"elements"` // note: nested elements
}

// flattenInteractiveRows normalises the two element layouts Feishu emits:
// rows of cells ([[elem...], ...]) and a flat cell list ([elem...]).
func flattenInteractiveRows(raw json.RawMessage) []json.RawMessage {
	if len(raw) == 0 {
		return nil
	}
	var rows [][]json.RawMessage
	if err := json.Unmarshal(raw, &rows); err == nil {
		var flat []json.RawMessage
		for _, row := range rows {
			flat = append(flat, row...)
		}
		return flat
	}
	var flat []json.RawMessage
	if err := json.Unmarshal(raw, &flat); err == nil {
		return flat
	}
	return nil
}

// interactiveOptionLabels renders select_static/overflow options. The
// receive format documents plain strings, but object options are tolerated.
func interactiveOptionLabels(raw json.RawMessage) []string {
	if len(raw) == 0 {
		return nil
	}
	var strs []string
	if err := json.Unmarshal(raw, &strs); err == nil {
		return strs
	}
	var objs []struct {
		Text  string `json:"text"`
		Value string `json:"value"`
	}
	if err := json.Unmarshal(raw, &objs); err == nil {
		var out []string
		for _, o := range objs {
			if o.Text != "" {
				out = append(out, o.Text)
			} else if o.Value != "" {
				out = append(out, o.Value)
			}
		}
		return out
	}
	return nil
}

// appendInteractiveReceiveElement renders one receive-format element. At
// mentions are emitted as their "@_user_N" token so the caller can resolve
// them through the event mentions list (stripMentions/replaceMentions),
// exactly like the text-message path does.
func appendInteractiveReceiveElement(raw json.RawMessage, parts *[]string, imageKeys *[]string) {
	var elem interactiveReceiveElement
	if err := json.Unmarshal(raw, &elem); err != nil {
		return
	}
	switch elem.Tag {
	case "text":
		if elem.Text != "" {
			*parts = append(*parts, elem.Text)
		}
	case "a":
		switch {
		case elem.Text != "" && elem.Href != "":
			*parts = append(*parts, fmt.Sprintf("[%s](%s)", elem.Text, elem.Href))
		case elem.Text != "":
			*parts = append(*parts, elem.Text)
		case elem.Href != "":
			*parts = append(*parts, elem.Href)
		}
	case "at":
		switch {
		case elem.UserID == "all":
			*parts = append(*parts, "@all")
		case elem.UserID != "":
			*parts = append(*parts, elem.UserID)
		case elem.UserName != "":
			*parts = append(*parts, "@"+elem.UserName)
		}
	case "button":
		if elem.Text != "" {
			*parts = append(*parts, elem.Text)
		}
	case "img":
		if elem.ImageKey != "" {
			*imageKeys = append(*imageKeys, elem.ImageKey)
		}
	case "hr":
		*parts = append(*parts, "---")
	case "note":
		for _, nested := range flattenInteractiveRows(elem.Elements) {
			appendInteractiveReceiveElement(nested, parts, imageKeys)
		}
	case "select_static", "overflow":
		if elem.Placeholder != "" {
			*parts = append(*parts, elem.Placeholder)
		}
		if labels := interactiveOptionLabels(elem.Options); len(labels) > 0 {
			*parts = append(*parts, "[options: "+strings.Join(labels, ", ")+"]")
		}
	case "date_picker":
		switch {
		case elem.Placeholder != "":
			*parts = append(*parts, elem.Placeholder)
		case elem.InitialDate != "":
			*parts = append(*parts, elem.InitialDate)
		default:
			*parts = append(*parts, "[date picker]")
		}
	default:
		// Unknown tags: keep any text payload instead of dropping the
		// element silently, so new Feishu element kinds still reach the
		// agent as (partial) text.
		if elem.Text != "" {
			*parts = append(*parts, elem.Text)
		}
	}
}

// extractInteractiveReceiveParts parses an inbound interactive message body
// in Feishu's simplified receive format. Returns readable text parts plus
// image_keys for the caller to download. Returns nil, nil when the body is
// not in receive format (the caller falls back to extractInteractiveCardText,
// which covers schema 2.0 and raw_card_content payloads).
func extractInteractiveReceiveParts(content string) (parts []string, imageKeys []string) {
	var card struct {
		Title    string          `json:"title"`
		Elements json.RawMessage `json:"elements"`
	}
	if err := json.Unmarshal([]byte(content), &card); err != nil {
		return nil, nil
	}
	// Tolerate locale-wrapped bodies ({"zh_cn": {...}}) the same way the
	// post parser does: retry with the first object value that parses.
	if len(card.Elements) == 0 && card.Title == "" {
		var wrapper map[string]json.RawMessage
		if err := json.Unmarshal([]byte(content), &wrapper); err == nil {
			for _, v := range wrapper {
				var inner struct {
					Title    string          `json:"title"`
					Elements json.RawMessage `json:"elements"`
				}
				if json.Unmarshal(v, &inner) == nil && (len(inner.Elements) > 0 || inner.Title != "") {
					card.Title, card.Elements = inner.Title, inner.Elements
					break
				}
			}
		}
	}
	if len(card.Elements) == 0 && card.Title == "" {
		return nil, nil
	}
	if card.Title != "" {
		parts = append(parts, card.Title)
	}
	for _, raw := range flattenInteractiveRows(card.Elements) {
		appendInteractiveReceiveElement(raw, &parts, &imageKeys)
	}
	return parts, imageKeys
}

// extractInteractiveReceiveText renders an inbound interactive card body to
// plain text without network access. Image slots become "[image]" markers,
// matching the post-quote convention. Falls back to extractInteractiveCardText
// (schema 2.0 / raw_card_content / legacy) when the body is not in receive
// format. Returns "" only when nothing readable could be extracted.
func extractInteractiveReceiveText(content string) string {
	parts, imageKeys := extractInteractiveReceiveParts(content)
	if len(parts) == 0 && len(imageKeys) == 0 {
		return extractInteractiveCardText(content)
	}
	for range imageKeys {
		parts = append(parts, "[image]")
	}
	return strings.Join(parts, "\n")
}

// downloadInteractiveImages fetches card image_keys best-effort: a failed
// image is logged and skipped so one expired image_key does not drop the
// whole card turn.
func (p *Platform) downloadInteractiveImages(messageID string, imageKeys []string) []core.ImageAttachment {
	var images []core.ImageAttachment
	for _, key := range imageKeys {
		if key == "" {
			continue
		}
		data, mimeType, err := p.downloadImage(messageID, key)
		if err != nil {
			slog.Error(p.tag()+": download interactive image failed", "error", err, "key", key)
			continue
		}
		images = append(images, core.ImageAttachment{MimeType: mimeType, Data: data})
	}
	return images
}

// parseInteractiveContent parses an inbound interactive (card) message into
// text parts and downloaded images for live dispatch. @bot tokens are left
// for the caller to strip via stripMentions, mirroring the text/post flow.
// Falls back to extractInteractiveCardText when the body is not in receive
// format (e.g. a raw schema-2.0 card delivered verbatim).
func (p *Platform) parseInteractiveContent(messageID, raw string, mentions []*larkim.MentionEvent) ([]string, []core.ImageAttachment) {
	parts, imageKeys := extractInteractiveReceiveParts(raw)
	if len(parts) == 0 && len(imageKeys) == 0 {
		if fallback := extractInteractiveCardText(raw); fallback != "" {
			return []string{stripMentions(fallback, mentions, p.getBotOpenID())}, nil
		}
		return nil, nil
	}
	return parts, p.downloadInteractiveImages(messageID, imageKeys)
}
