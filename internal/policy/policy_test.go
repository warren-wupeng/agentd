package policy

import (
	"encoding/json"
	"testing"
)

func check(t *testing.T, engine *Static, input, tool string, want Decision) {
	t.Helper()
	v := engine.Check(tool, json.RawMessage(input))
	if v.Decision != want {
		t.Fatalf("Check(%q, %s) = %s (%s), want %s", tool, input, v.Decision, v.Reason, want)
	}
}

func TestStaticBashRules(t *testing.T) {
	e := NewStatic()
	check(t, e, `{"command":"ls -la"}`, "bash", Allow)
	check(t, e, `{"command":"cat notes.txt && grep x y"}`, "bash", Allow)
	check(t, e, `{"command":"rm -rf ./build"}`, "bash", Allow) // relative target is fine
	check(t, e, `{"command":"sudo ls"}`, "bash", Deny)
	check(t, e, `{"command":"cd /tmp && sudo rm x"}`, "bash", Deny)
	check(t, e, `{"command":"rm -rf /"}`, "bash", Deny)
	check(t, e, `{"command":"rm  -fr  /*"}`, "bash", Deny)
	check(t, e, `{"command":"echo not really sudo"}`, "bash", Allow) // substring must not trip
}

func TestStaticIgnoresNonBash(t *testing.T) {
	e := NewStatic()
	check(t, e, `{"path":"../../etc/passwd"}`, "write_file", Allow)
}

func TestStaticUnparseableInputIsAllow(t *testing.T) {
	// unparseable input is the tool's error, not policy's
	e := NewStatic()
	v := e.Check("bash", json.RawMessage(`not json`))
	if v.Decision != Allow {
		t.Fatalf("unparseable input denied: %+v", v)
	}
}

func TestStaticAskRules(t *testing.T) {
	e := NewStatic()
	check(t, e, `{"command":"git push origin main"}`, "bash", Ask)
	check(t, e, `{"command":"git push"}`, "bash", Ask)
	check(t, e, `{"command":"git commit -m x && git push"}`, "bash", Ask)
	check(t, e, `{"command":"git pull"}`, "bash", Allow)
	check(t, e, `{"command":"echo git push"}`, "bash", Allow) // substring must not trip
}
