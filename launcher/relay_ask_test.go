package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"
)

// fakeAI stands in for llama-server's OpenAI-style API.
type fakeAI struct {
	mu    sync.Mutex
	calls []map[string]any
}

func (f *fakeAI) server(t *testing.T) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			w.Write([]byte(`{"status":"ok"}`))
			return
		}
		if r.URL.Path != "/v1/chat/completions" {
			http.NotFound(w, r)
			return
		}
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		f.calls = append(f.calls, body)
		f.mu.Unlock()
		msgs := body["messages"].([]any)
		system := msgs[0].(map[string]any)["content"].(string)
		user := msgs[1].(map[string]any)["content"].(string)
		var reply string
		switch {
		case strings.Contains(system, "into searches"):
			reply = `{"searches":["bleach","water"]}`
		case strings.Contains(system, "librarian"):
			n := "1"
			for _, line := range strings.Split(user, "\n") {
				if m := regexp.MustCompile(`^(\d+)\. Emergency Water Treatment.*page 3`).FindStringSubmatch(line); m != nil {
					n = m[1]
				}
			}
			reply = "```json\n{\"best\":[" + n + "]}\n```"
		default:
			if !strings.Contains(user, "8 drops") {
				reply = "Not covered here."
			} else {
				reply = "<think>hmm</think>**Use 8 drops** of plain bleach per gallon of clear water.\n- Wait 30 minutes before drinking."
			}
		}
		json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]any{"content": reply}}}})
	}))
}

func TestRelayAsk(t *testing.T) {
	ks := fakeKiwix(t)
	defer ks.Close()
	fa := &fakeAI{}
	as := fa.server(t)
	defer as.Close()
	r, node := startRelay(t, ks.URL+"/kiwix", []string{wikiBook, survBook})
	r.pdf = newTestPdfLibrary(t, ks.URL+"/kiwix")
	r.ai = &aiEngine{url: as.URL, client: http.DefaultClient, state: "off"}

	node.receive(teamNode, "the creek water looks dirty, how much bleach do I add?")
	got := node.replies()
	all := strings.Join(got, "\n")
	if len(got) < 2 || !strings.HasPrefix(got[0], "🤖 Use 8 drops of plain bleach per gallon of clear water. Wait 30 minutes before drinking. (from 1)") {
		t.Fatalf("ask answer: %q", got)
	}
	if !strings.Contains(all, "🔎 the creek water looks dirty, how much…\n1 Emergency Water Treatment p.3 [Water]") {
		t.Fatalf("ask sources: %q", got)
	}
	checkSizes(t, got, 200)
	fa.mu.Lock()
	if len(fa.calls) != 3 {
		t.Fatalf("%d AI calls, want 3", len(fa.calls))
	}
	if k, _ := fa.calls[0]["chat_template_kwargs"].(map[string]any); k["enable_thinking"] != false {
		t.Fatalf("thinking not turned off: %v", fa.calls[0])
	}
	if _, ok := fa.calls[0]["response_format"]; !ok {
		t.Fatalf("no JSON schema sent")
	}
	fa.mu.Unlock()

	// ?1 opens the page the AI picked; ?more pages through the rest of its list.
	node.receive(teamNode, "?1")
	got = node.replies()
	if len(got) == 0 || !strings.Contains(got[0], "§3 p.3\nDisinfecting with bleach") {
		t.Fatalf("?1 after ask: %q", got)
	}

	// Sources only: no written answer.
	cfg := r.Config()
	cfg.AI = aiModeSources
	r.SetConfig(cfg)
	node.receive(teamNode, "?ask how much bleach for water")
	got = node.replies()
	if len(got) == 0 || strings.HasPrefix(got[0], "🤖") || !strings.Contains(got[0], "1 Emergency Water Treatment p.3") {
		t.Fatalf("sources only: %q", got)
	}

	// AI off: ?ask is a keyword search, and plain words stay a search.
	cfg.AI = aiModeOff
	r.SetConfig(cfg)
	node.receive(teamNode, "?ask how much bleach do I need")
	got = node.replies()
	if len(got) == 0 || !strings.HasPrefix(got[0], "🔎 bleach\n1 Emergency") {
		t.Fatalf("ask with AI off: %q", got)
	}
}

func TestAskKeywords(t *testing.T) {
	for q, want := range map[string]string{
		"I have a small cut, walk me through stitching it up": "small cut stitching",
		"How do I purify water?":                              "purify water",
		"what's":                                              "what's",
	} {
		if got := askKeywords(q); got != want {
			t.Errorf("askKeywords(%q) = %q, want %q", q, got, want)
		}
	}
}

func TestTidyAnswer(t *testing.T) {
	got := tidyAnswer("## Steps\n1. Clean it.\n2. Cover it. "+strings.Repeat("More words here. ", 30), 120)
	if len(got) > 120 || !strings.HasPrefix(got, "Steps 1. Clean it. 2. Cover it.") || !strings.HasSuffix(got, ".") {
		t.Fatalf("tidy: %q", got)
	}
}

func TestDropCallForHelp(t *testing.T) {
	in := "Press firmly on the wound for 10 minutes. Call 911 if bleeding doesn't stop. Seek immediate medical attention. Raise the limb above the heart. You should see a doctor for stitches. Go to the ER. Watch for redness and swelling."
	want := "Press firmly on the wound for 10 minutes. Raise the limb above the heart. Watch for redness and swelling."
	if got := dropCallForHelp(in); got != want {
		t.Fatalf("got %q", got)
	}
	if got := tidyAnswer("1. Remove the stinger. 2. Call 911 now.", 300); got != "1. Remove the stinger." {
		t.Fatalf("tidy: %q", got)
	}
}

func TestPickAIModel(t *testing.T) {
	if m := pickAIModel(16 << 30); m == nil || m.Repo != "Qwen/Qwen3-4B-GGUF" {
		t.Fatalf("16 GB: %+v", m)
	}
	if m := pickAIModel(6 << 30); m == nil || m.Repo != "Qwen/Qwen3-1.7B-GGUF" {
		t.Fatalf("6 GB: %+v", m)
	}
	if m := pickAIModel(2 << 30); m != nil {
		t.Fatalf("2 GB should be too small: %+v", m)
	}
}
