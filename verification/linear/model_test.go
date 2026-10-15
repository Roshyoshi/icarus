package main

import (
	"testing"
	"time"

	"github.com/anishathalye/porcupine"
)

type fixture struct {
	calls  []porcupine.Operation
	serial int
}

func (f *fixture) add(req Request, result object) {
	f.serial++
	f.calls = append(f.calls, porcupine.Operation{Input: req, Output: Observed{Kind: "response", Response: Response{Version: 1, OK: true, Result: result}}, Call: int64(f.serial * 10), Return: int64(f.serial*10 + 5)})
}
func request(id, method string, fields object) Request {
	c := object{"method": method}
	for k, v := range fields {
		c[k] = v
	}
	return Request{Version: 1, OperationID: id, Member: "a", Launch: "l1", Command: c}
}
func base() *fixture {
	f := &fixture{}
	f.add(Request{Version: 1, OperationID: "launch1", Command: object{"method": "launch_start", "provider": "codex", "instance": "i"}}, object{"launch": "l1"})
	f.add(Request{Version: 1, OperationID: "bind1", Launch: "l1", Command: object{"method": "bind", "session": "s"}}, object{"member": "a", "launch": "l1", "parent": nil})
	return f
}
func check(t *testing.T, f *fixture, want porcupine.CheckResult) {
	t.Helper()
	got, _ := porcupine.CheckOperationsVerbose(specification(), f.calls, time.Second)
	if got != want {
		t.Fatalf("got %s want %s\n%s", got, want, canonical(f.calls))
	}
}
func cas(key string, revision int, value any, swapped bool) object {
	return object{"swapped": swapped, "cell": object{"key": key, "revision": revision, "value": value}}
}
func message(id, body string, sequence int) object {
	return object{"message": id, "sequence": sequence, "from": "a", "body": body}
}
func inbox(messages []object, acked, end int) object {
	return object{"messages": messages, "acked": acked, "batch_end": end}
}

func TestAcceptsConcurrentCASAndReplay(t *testing.T) {
	f := base()
	q := request("cas1", "cell_cas", object{"key": "x", "expected": 0, "value": "one"})
	f.add(q, cas("x", 1, "one", true))
	f.add(request("cas2", "cell_cas", object{"key": "x", "expected": 0, "value": "two"}), cas("x", 1, "one", false))
	f.calls[2].Return = 50
	f.calls[3].Call = 31
	f.add(q, cas("x", 1, "one", true))
	check(t, f, porcupine.Ok)
}
func TestRejectsSpecificProtocolLies(t *testing.T) {
	cases := map[string]func(*fixture){
		"double CAS success": func(f *fixture) {
			f.add(request("c1", "cell_cas", object{"key": "x", "expected": 0, "value": "a"}), cas("x", 1, "a", true))
			f.add(request("c2", "cell_cas", object{"key": "x", "expected": 0, "value": "b"}), cas("x", 1, "b", true))
			f.calls[2].Return = 50
			f.calls[3].Call = 31
		},
		"replay duplicates effect": func(f *fixture) {
			q := request("send", "send", object{"to": "a", "body": "hi"})
			f.add(q, object{"message": "m1", "sequence": 1})
			f.add(q, object{"message": "m2", "sequence": 2})
		},
		"changed arguments reuse op": func(f *fixture) {
			q := request("send", "send", object{"to": "a", "body": "hi"})
			f.add(q, object{"message": "m1", "sequence": 1})
			q.Command = object{"method": "send", "to": "a", "body": "changed"}
			f.add(q, object{"message": "m1", "sequence": 1})
		},
		"mailbox reordered": func(f *fixture) {
			f.add(request("s1", "send", object{"to": "a", "body": "one"}), object{"message": "m1", "sequence": 1})
			f.add(request("s2", "send", object{"to": "a", "body": "two"}), object{"message": "m2", "sequence": 2})
			f.add(request("fetch", "inbox", object{}), inbox([]object{message("m2", "two", 2), message("m1", "one", 1)}, 0, 2))
		},
		"ack before fetch": func(f *fixture) {
			f.add(request("s1", "send", object{"to": "a", "body": "one"}), object{"message": "m1", "sequence": 1})
			f.add(request("ack", "ack", object{"through": "m1"}), object{"acked": 1})
		},
		"terminal rewrite": func(f *fixture) {
			f.add(request("t1", "task_create", object{"owner": "a", "description": "work"}), object{"task": "t"})
			for i, v := range []string{"first", "changed"} {
				result := object{"status": "success", "value": v}
				f.add(request([]string{"f1", "f2"}[i], "task_finish", object{"task": "t", "result": result}), object{"task": "t", "owner": "a", "description": "work", "result": result})
			}
		},
		"stale generation accepted": func(f *fixture) {
			f.add(Request{Version: 1, OperationID: "launch2", Command: object{"method": "launch_start", "provider": "codex", "instance": "i"}}, object{"launch": "l2"})
			f.add(Request{Version: 1, OperationID: "bind2", Launch: "l2", Command: object{"method": "bind", "session": "s"}}, object{"member": "a", "launch": "l2", "parent": nil})
			f.add(request("read", "cell_read", object{"key": "x"}), object{"key": "x", "revision": 0, "value": nil})
		},
	}
	for name, build := range cases {
		t.Run(name, func(t *testing.T) { f := base(); build(f); check(t, f, porcupine.Illegal) })
	}
}
func TestCrashSemanticsBoundEffectsBeforeDeath(t *testing.T) {
	for _, kind := range []string{"known_abort", "known_commit", "unknown"} {
		for _, observedBeforeReplay := range []bool{false, true} {
			t.Run(kind+map[bool]string{false: "-absent", true: "-present"}[observedBeforeReplay], func(t *testing.T) {
				f := base()
				q := request("ambiguous", "send", object{"to": "a", "body": "hi"})
				candidate := Response{Version: 1, OK: true, Result: object{"message": "m", "sequence": 1}}
				f.serial++
				f.calls = append(f.calls, porcupine.Operation{Input: q, Output: Observed{Kind: kind, Candidate: &candidate}, Call: 30, Return: 35})
				messages := []object{}
				end := 0
				if observedBeforeReplay {
					messages = append(messages, message("m", "hi", 1))
					end = 1
				}
				f.add(request("before-replay", "inbox", object{}), inbox(messages, 0, end))
				f.add(q, candidate.Result)
				want := porcupine.Ok
				if (kind == "known_abort" && observedBeforeReplay) || (kind == "known_commit" && !observedBeforeReplay) {
					want = porcupine.Illegal
				}
				check(t, f, want)
			})
		}
	}
}
func TestPartialAckAndHistoricalInboxReplay(t *testing.T) {
	f := base()
	for i, id := range []string{"m1", "m2"} {
		f.add(request(id, "send", object{"to": "a", "body": id}), object{"message": id, "sequence": i + 1})
	}
	q := request("fetch", "inbox", object{})
	batch := inbox([]object{message("m1", "m1", 1), message("m2", "m2", 2)}, 0, 2)
	f.add(q, batch)
	f.add(request("ack", "ack", object{"through": "m1"}), object{"acked": 1})
	f.add(request("next", "inbox", object{"limit": 1}), inbox([]object{message("m2", "m2", 2)}, 1, 2))
	f.add(q, batch)
	check(t, f, porcupine.Ok)
}

func TestKnownCommitCannotBeExplainedByErrorCandidate(t *testing.T) {
	f := base()
	q := request("impossible-commit", "send", object{"to": "a", "body": "hi"})
	q.Launch = "not-the-current-launch"
	candidate := Response{Version: 1, OK: false, Error: "unknown member or stale launch"}
	f.calls = append(f.calls, porcupine.Operation{Input: q, Output: Observed{Kind: "known_commit", Candidate: &candidate}, Call: 30, Return: 35})
	check(t, f, porcupine.Illegal)
}
