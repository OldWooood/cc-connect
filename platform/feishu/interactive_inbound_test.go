package feishu

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chenhg5/cc-connect/core"

	lark "github.com/larksuite/oapi-sdk-go/v3"
	larkim "github.com/larksuite/oapi-sdk-go/v3/service/im/v1"
)

const testInteractiveBotID = "ou_interactive_bot"

// testInteractiveCardContent is a receive-format card covering every element
// tag the parser supports, modelled on the Feishu "Receive message content
// structure" doc example.
const testInteractiveCardContent = `{
  "title": "周报卡片",
  "elements": [
    [{"tag": "text", "text": "本周进展如下"}],
    [
      {"tag": "a", "href": "https://example.com/report", "text": "查看详情"},
      {"tag": "at", "user_id": "@_user_1", "user_name": ""},
      {"tag": "at", "user_id": "all"}
    ],
    [
      {"tag": "button", "text": "批准", "type": "primary"},
      {"tag": "button", "text": "驳回", "type": "danger"}
    ],
    [{"tag": "hr"}],
    [
      {"tag": "text", "text": "截图"},
      {"tag": "img", "image_key": "img_test_key_1"}
    ],
    [{"tag": "note", "elements": [{"tag": "text", "text": "备注信息"}]}],
    [{"tag": "select_static", "placeholder": "请选择负责人", "options": ["张三", "李四"]}],
    [{"tag": "overflow", "options": ["打开目录", "查看文档"]}],
    [{"tag": "date_picker", "placeholder": "请选择日期", "initial_date": "2026-10-01"}]
  ]
}`

func TestExtractInteractiveReceiveParts_FullCard(t *testing.T) {
	parts, imageKeys := extractInteractiveReceiveParts(testInteractiveCardContent)

	wantParts := []string{
		"周报卡片",
		"本周进展如下",
		"[查看详情](https://example.com/report)",
		"@_user_1",
		"@all",
		"批准",
		"驳回",
		"---",
		"截图",
		"备注信息",
		"请选择负责人",
		"[options: 张三, 李四]",
		"[options: 打开目录, 查看文档]",
		"请选择日期",
	}
	if len(parts) != len(wantParts) {
		t.Fatalf("parts = %q, want %q", parts, wantParts)
	}
	for i := range wantParts {
		if parts[i] != wantParts[i] {
			t.Fatalf("parts[%d] = %q, want %q (full: %q)", i, parts[i], wantParts[i], parts)
		}
	}
	if len(imageKeys) != 1 || imageKeys[0] != "img_test_key_1" {
		t.Fatalf("imageKeys = %q, want [img_test_key_1]", imageKeys)
	}
}

func TestExtractInteractiveReceiveParts_FlatElements(t *testing.T) {
	parts, imageKeys := extractInteractiveReceiveParts(
		`{"title":"T","elements":[{"tag":"text","text":"hi"},{"tag":"img","image_key":"k1"}]}`)
	if len(parts) != 2 || parts[0] != "T" || parts[1] != "hi" {
		t.Fatalf("parts = %q", parts)
	}
	if len(imageKeys) != 1 || imageKeys[0] != "k1" {
		t.Fatalf("imageKeys = %q", imageKeys)
	}
}

func TestExtractInteractiveReceiveParts_InvalidJSON(t *testing.T) {
	for _, in := range []string{"", "{not json", `{"foo":1}`, `{"title":"","elements":[]}`} {
		if parts, keys := extractInteractiveReceiveParts(in); len(parts) != 0 || len(keys) != 0 {
			t.Fatalf("extractInteractiveReceiveParts(%q) = %q, %q; want empty", in, parts, keys)
		}
	}
}

func TestExtractInteractiveReceiveText_ImagePlaceholderAndFallback(t *testing.T) {
	got := extractInteractiveReceiveText(`{"title":"T","elements":[[{"tag":"img","image_key":"k"}]]}`)
	if got != "T\n[image]" {
		t.Fatalf("got %q, want %q", got, "T\n[image]")
	}
	// Not receive format and not a card either: legacy placeholder preserved.
	if got := extractInteractiveReceiveText(`{not json`); got != "[interactive card]" {
		t.Fatalf("got %q, want [interactive card]", got)
	}
}

func TestIsGroupHistoryMessageType_Interactive(t *testing.T) {
	if !isGroupHistoryMessageType("interactive") {
		t.Fatal("interactive should be a group-history message type")
	}
}

func TestHistoryText_InteractiveStripsBotMention(t *testing.T) {
	p := &Platform{platformName: "feishu", botOpenID: testInteractiveBotID}
	mentions := []*larkim.MentionEvent{
		{Key: strPtr("@_user_1"), Id: &larkim.UserId{OpenId: strPtr(testInteractiveBotID)}},
		{Key: strPtr("@_user_2"), Id: &larkim.UserId{OpenId: strPtr("ou_other")}, Name: strPtr("李四")},
	}
	content := `{"title":"审批单","elements":[[{"tag":"text","text":"请审批"}],[{"tag":"at","user_id":"@_user_1"},{"tag":"at","user_id":"@_user_2"}]]}`
	got := p.historyText("interactive", content, mentions)
	// Bot token removed, other user resolved to @name.
	if strings.Contains(got, "@_user_1") || strings.Contains(got, testInteractiveBotID) {
		t.Fatalf("bot mention not stripped: %q", got)
	}
	if !strings.Contains(got, "@李四") || !strings.Contains(got, "请审批") {
		t.Fatalf("unexpected history text: %q", got)
	}
}

func TestHistoryText_InteractiveUnparseable(t *testing.T) {
	p := &Platform{platformName: "feishu"}
	if got := p.historyText("interactive", `{not json`, nil); got != "" {
		t.Fatalf("got %q, want empty (marker must not pollute history)", got)
	}
}

// TestDispatchMessageInteractive covers live dispatch of a card: title, text,
// link, @bot (stripped), other @user (kept as @name), buttons, and one image
// downloaded through the resource stub.
func TestDispatchMessageInteractive(t *testing.T) {
	const appID = "cli_interactive"
	const appSecret = "secret-interactive"
	const messageID = "om_interactive_1"
	const imageKey = "img_interactive_1"
	imgBytes := []byte("\x89PNG\r\n\x1a\nfake-png-bytes")

	got := make(chan *core.Message, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/open-apis/auth/v3/tenant_access_token/internal":
			w.Header().Set("Content-Type", "application/json")
			writeJSON(t, w, map[string]any{
				"code":                0,
				"msg":                 "success",
				"expire":              7200,
				"tenant_access_token": "tenant-token",
			})
		case r.URL.Path == "/open-apis/im/v1/messages/"+messageID+"/resources/"+imageKey:
			if r.URL.Query().Get("type") != "image" {
				t.Fatalf("resource type = %q, want image", r.URL.Query().Get("type"))
			}
			if _, err := w.Write(imgBytes); err != nil {
				t.Fatalf("write image: %v", err)
			}
		case strings.HasPrefix(r.URL.Path, "/open-apis/contact/v3/users/"):
			w.Header().Set("Content-Type", "application/json")
			writeJSON(t, w, map[string]any{"code": 0, "msg": "success"})
		case strings.HasPrefix(r.URL.Path, "/open-apis/im/v1/chats/"):
			w.Header().Set("Content-Type", "application/json")
			writeJSON(t, w, map[string]any{"code": 0, "msg": "success"})
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	}))
	defer srv.Close()

	p := &Platform{
		platformName:         "feishu",
		domain:               srv.URL,
		appID:                appID,
		appSecret:            appSecret,
		botOpenID:            testInteractiveBotID,
		resourceDownloadHTTP: srv.Client(),
		client: lark.NewClient(appID, appSecret,
			lark.WithOpenBaseUrl(srv.URL),
			lark.WithHttpClient(srv.Client()),
		),
		handler: func(_ core.Platform, msg *core.Message) {
			got <- msg
		},
	}

	mentions := []*larkim.MentionEvent{
		{Key: strPtr("@_user_1"), Id: &larkim.UserId{OpenId: strPtr(testInteractiveBotID)}},
		{Key: strPtr("@_user_2"), Id: &larkim.UserId{OpenId: strPtr("ou_other")}, Name: strPtr("李四")},
	}
	content := `{"title":"审批单","elements":[
		[{"tag":"text","text":"请审批这个需求"}],
		[{"tag":"a","href":"https://example.com/spec","text":"需求文档"}],
		[{"tag":"at","user_id":"@_user_1"},{"tag":"at","user_id":"@_user_2"}],
		[{"tag":"button","text":"批准","type":"primary"}],
		[{"tag":"img","image_key":"` + imageKey + `"}]
	]}`
	p.dispatchMessage(
		context.Background(),
		"interactive",
		content,
		mentions,
		messageID,
		"feishu:oc_chat:ou_user",
		"ou_user",
		"oc_chat",
		replyContext{messageID: messageID, sessionKey: "feishu:oc_chat:ou_user"},
		"",
		0,
	)

	select {
	case msg := <-got:
		for _, want := range []string{"审批单", "请审批这个需求", "[需求文档](https://example.com/spec)", "@李四", "批准"} {
			if !strings.Contains(msg.Content, want) {
				t.Fatalf("content %q missing %q", msg.Content, want)
			}
		}
		if strings.Contains(msg.Content, "@_user_1") {
			t.Fatalf("bot mention not stripped: %q", msg.Content)
		}
		if len(msg.Images) != 1 {
			t.Fatalf("images = %d, want 1", len(msg.Images))
		}
	case <-time.After(5 * time.Second):
		t.Fatal("expected interactive message dispatch")
	}
}

// TestOnMessage_InteractiveGroupMention verifies the full inbound path for
// cards in group chats: @bot cards reach the handler, unmentioned cards do
// not (same contract as text messages).
func TestOnMessage_InteractiveGroupMention(t *testing.T) {
	newGroupPlatform := func(handler core.MessageHandler) *interactivePlatform {
		platformAny, err := New(map[string]any{"app_id": "cli_xxx", "app_secret": "secret"})
		if err != nil {
			t.Fatalf("New() error = %v", err)
		}
		ip, ok := platformAny.(*interactivePlatform)
		if !ok {
			t.Fatalf("platform type = %T, want *interactivePlatform", platformAny)
		}
		ip.botOpenID = testInteractiveBotID
		ip.handler = handler
		return ip
	}

	makeEvent := func(messageID, content string, mentions []*larkim.MentionEvent) *larkim.P2MessageReceiveV1 {
		msgType, chatType, senderType := "interactive", "group", "user"
		openID, chatID := "ou_sender", "oc_group"
		create := strconv.FormatInt(time.Now().UnixMilli(), 10)
		return &larkim.P2MessageReceiveV1{
			Event: &larkim.P2MessageReceiveV1Data{
				Sender: &larkim.EventSender{
					SenderId:   &larkim.UserId{OpenId: &openID},
					SenderType: &senderType,
				},
				Message: &larkim.EventMessage{
					MessageId:   &messageID,
					ChatId:      &chatID,
					ChatType:    &chatType,
					MessageType: &msgType,
					Content:     &content,
					Mentions:    mentions,
					CreateTime:  &create,
				},
			},
		}
	}

	botMention := []*larkim.MentionEvent{
		{Key: strPtr("@_user_1"), Id: &larkim.UserId{OpenId: strPtr(testInteractiveBotID)}},
	}
	cardWithMention := `{"title":"审批单","elements":[[{"tag":"text","text":"请审批"}],[{"tag":"at","user_id":"@_user_1"}]]}`
	cardPlain := `{"title":"周报","elements":[[{"tag":"text","text":"本周进展"}]]}`

	t.Run("mentioned card reaches handler", func(t *testing.T) {
		var wg sync.WaitGroup
		wg.Add(1)
		var received *core.Message
		ip := newGroupPlatform(func(_ core.Platform, msg *core.Message) {
			defer wg.Done()
			received = msg
		})
		if err := ip.onMessage(context.Background(), makeEvent("om_mention_1", cardWithMention, botMention)); err != nil {
			t.Fatalf("onMessage() error = %v", err)
		}
		done := make(chan struct{})
		go func() { wg.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("expected handler invocation for @bot card")
		}
		if received == nil {
			t.Fatal("expected handler to receive a message")
		}
		if !strings.Contains(received.Content, "请审批") {
			t.Fatalf("content = %q, want card text", received.Content)
		}
		if strings.Contains(received.Content, "@_user_1") {
			t.Fatalf("bot mention not stripped: %q", received.Content)
		}
	})

	t.Run("unmentioned card is dropped", func(t *testing.T) {
		called := make(chan struct{}, 1)
		ip := newGroupPlatform(func(_ core.Platform, _ *core.Message) {
			called <- struct{}{}
		})
		if err := ip.onMessage(context.Background(), makeEvent("om_plain_1", cardPlain, nil)); err != nil {
			t.Fatalf("onMessage() error = %v", err)
		}
		select {
		case <-called:
			t.Fatal("unmentioned group card must not reach handler")
		case <-time.After(500 * time.Millisecond):
		}
	})
}
