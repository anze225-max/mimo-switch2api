package wire

import (
	"encoding/json"
	"testing"
)

func TestAnthropicImageBecomesContentParts(t *testing.T) {
	parsed := translate(t, `{
	  "model":"mimo-v2.6-flash","max_tokens":8,
	  "messages":[{"role":"user","content":[
	    {"type":"text","text":"什么颜色？"},
	    {"type":"image","source":{"type":"base64","media_type":"image/png","data":"AAAA"}},
	    {"type":"image","source":{"type":"url","url":"https://example.test/x.jpg"}}]}]}`)

	user := parsed["messages"].([]any)[0].(map[string]any)
	parts, ok := user["content"].([]any)
	if !ok {
		t.Fatalf("content = %T %v, want part array", user["content"], user["content"])
	}
	if len(parts) != 3 {
		t.Fatalf("want text + 2 images, got %d: %s", len(parts), mustJSON(t, parts))
	}
	if parts[0].(map[string]any)["text"] != "什么颜色？" {
		t.Errorf("text part = %v", parts[0])
	}
	want := []string{"data:image/png;base64,AAAA", "https://example.test/x.jpg"}
	for i, url := range want {
		img := parts[i+1].(map[string]any)
		if img["type"] != "image_url" {
			t.Errorf("part %d type = %v", i+1, img["type"])
			continue
		}
		if got := img["image_url"].(map[string]any)["url"]; got != url {
			t.Errorf("part %d url = %v, want %s", i+1, got, url)
		}
	}
}

func TestAnthropicImageOnlyTurnKeepsTheImage(t *testing.T) {
	parsed := translate(t, `{
	  "model":"m","max_tokens":8,
	  "messages":[{"role":"user","content":[
	    {"type":"image","source":{"type":"base64","media_type":"image/jpeg","data":"QQ"}}]}]}`)

	parts := parsed["messages"].([]any)[0].(map[string]any)["content"].([]any)
	if len(parts) != 1 {
		t.Fatalf("want a lone image part, got %s", mustJSON(t, parts))
	}
	if _, ok := parts[0].(map[string]any)["text"]; ok {
		t.Errorf("empty prose must not become a text part: %v", parts[0])
	}
}

func TestUnusableImageSourcesAreSkipped(t *testing.T) {
	parsed := translate(t, `{
	  "model":"m","max_tokens":8,
	  "messages":[{"role":"user","content":[
	    {"type":"text","text":"keep me"},
	    {"type":"image","source":{"type":"base64","media_type":"image/png"}},
	    {"type":"image","source":{"type":"url"}},
	    {"type":"image"}]}]}`)

	user := parsed["messages"].([]any)[0].(map[string]any)
	if user["content"] != "keep me" {
		t.Fatalf("content = %v, want the bare string since every image was malformed", user["content"])
	}
}

func TestImageURLConversions(t *testing.T) {
	cases := []struct {
		name string
		src  *ImageSource
		want string
		ok   bool
	}{
		{"nil source", nil, "", false},
		{"base64", &ImageSource{Type: "base64", MediaType: "image/png", Data: "AA=="}, "data:image/png;base64,AA==", true},
		{"base64 without media type", &ImageSource{Type: "base64", Data: "AA=="}, "", false},
		{"base64 without data", &ImageSource{Type: "base64", MediaType: "image/png"}, "", false},
		{"url", &ImageSource{Type: "url", URL: "https://a.test/b.png"}, "https://a.test/b.png", true},
		{"url empty", &ImageSource{Type: "url"}, "", false},
		{"unknown source type", &ImageSource{Type: "text", URL: "https://a.test/b.png"}, "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := imageURL(c.src)
			if got != c.want || ok != c.ok {
				t.Errorf("imageURL = %q,%v want %q,%v", got, ok, c.want, c.ok)
			}
		})
	}
}

func TestScreenshotInToolResultStillReachesTheModel(t *testing.T) {
	parsed := translate(t, `{
	  "model":"m","max_tokens":8,
	  "messages":[{"role":"user","content":[
	    {"type":"tool_result","tool_use_id":"call_1","content":[
	      {"type":"text","text":"captured"},
	      {"type":"image","source":{"type":"base64","media_type":"image/png","data":"QQ"}}]}]}]}`)

	messages := parsed["messages"].([]any)
	if len(messages) != 2 {
		t.Fatalf("want tool message + follow-up user turn, got %s", mustJSON(t, messages))
	}
	tool := messages[0].(map[string]any)
	if tool["role"] != "tool" || tool["content"] != "captured" {
		t.Errorf("tool message = %s", mustJSON(t, tool))
	}
	followUp := messages[1].(map[string]any)
	parts, ok := followUp["content"].([]any)
	if !ok || followUp["role"] != "user" || len(parts) != 1 {
		t.Fatalf("follow-up = %s", mustJSON(t, followUp))
	}
	url := parts[0].(map[string]any)["image_url"].(map[string]any)["url"]
	if url != "data:image/png;base64,QQ" {
		t.Errorf("url = %v", url)
	}
}

func TestResponsesInputImageSurvivesTheHop(t *testing.T) {
	body := `{
	  "model":"mimo-v2.6-pro",
	  "input":[{"role":"user","content":[
	    {"type":"input_text","text":"看这张图"},
	    {"type":"input_image","image_url":"data:image/png;base64,AA==","detail":"low"}]}]}`
	out, err := ResponsesToChat([]byte(body))
	if err != nil {
		t.Fatalf("ResponsesToChat: %v", err)
	}
	var parsed struct {
		Messages []struct {
			Role    string `json:"role"`
			Content any    `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(out, &parsed); err != nil {
		t.Fatal(err)
	}
	parts, ok := parsed.Messages[0].Content.([]any)
	if !ok {
		t.Fatalf("content = %T %v, want part array", parsed.Messages[0].Content, parsed.Messages[0].Content)
	}
	if len(parts) != 2 {
		t.Fatalf("want text + image, got %s", mustJSON(t, parts))
	}
	img := parts[1].(map[string]any)
	if img["type"] != "image_url" {
		t.Errorf("type = %v", img["type"])
	}
	url := img["image_url"].(map[string]any)
	if url["url"] != "data:image/png;base64,AA==" {
		t.Errorf("url = %v", url["url"])
	}
	if url["detail"] != "low" {
		t.Errorf("detail = %v", url["detail"])
	}
}

func TestResponsesTextOnlyContentStaysAString(t *testing.T) {
	out, err := ResponsesToChat([]byte(`{"model":"m","input":[
	  {"role":"user","content":[{"type":"input_text","text":"hi"}]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	var parsed struct {
		Messages []struct {
			Content any `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(out, &parsed); err != nil {
		t.Fatal(err)
	}
	if parsed.Messages[0].Content != "hi" {
		t.Errorf("content = %v, want the bare string", parsed.Messages[0].Content)
	}
}
