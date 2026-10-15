package main

import (
	"encoding/json"
	"fmt"
	"hash/fnv"
	"strings"

	"github.com/anishathalye/porcupine"
)

type object = map[string]any

type Request struct {
	Version     int    `json:"version"`
	OperationID string `json:"operation_id,omitempty"`
	Member      string `json:"member,omitempty"`
	Launch      string `json:"launch,omitempty"`
	Command     object `json:"command"`
}
type Response struct {
	Version int    `json:"version"`
	OK      bool   `json:"ok"`
	Result  object `json:"result,omitempty"`
	Error   string `json:"error,omitempty"`
}
type Observed struct {
	Kind      string    `json:"kind"` // response, known_abort, known_commit, unknown
	Response  Response  `json:"response"`
	Candidate *Response `json:"recovery_response,omitempty"`
	Exit      int       `json:"exit"`
	Stderr    string    `json:"stderr,omitempty"`
}
type Launch struct {
	Provider, Instance string
	Ordinal            int
}
type Member struct {
	Provider, Instance, Session, Agent, Parent, Launch string
	Ordinal                                            int
}
type Mailbox struct {
	Messages   []object
	Acked, End int
}
type Cell struct {
	Revision int
	Value    any
}
type Task struct {
	Owner, Description string
	Result             any
}
type Replay struct {
	Request string
	Result  object
}
type State struct {
	Launches    map[string]Launch
	Members     map[string]Member
	MemberOrder []string
	Mailboxes   map[string]Mailbox
	Cells       map[string]Cell
	Tasks       map[string]Task
	Replays     map[string]Replay
	Ordinal     int
}

func initial() string {
	return canonical(State{Launches: map[string]Launch{}, Members: map[string]Member{}, MemberOrder: []string{}, Mailboxes: map[string]Mailbox{}, Cells: map[string]Cell{}, Tasks: map[string]Task{}, Replays: map[string]Replay{}})
}
func canonical(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(b)
}
func decode[T any](v any) T {
	var r T
	if err := json.Unmarshal([]byte(canonical(v)), &r); err != nil {
		panic(err)
	}
	return r
}
func str(v any) string { s, _ := v.(string); return s }
func integer(v any) int {
	switch n := v.(type) {
	case int:
		return n
	case float64:
		return int(n)
	case int64:
		return int(n)
	}
	return 0
}
func same(a, b any) bool      { return canonical(a) == canonical(b) }
func method(r Request) string { return str(r.Command["method"]) }
func mutation(m string) bool  { return m != "members" && m != "cell_read" && m != "task_read" }
func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}
func valid(s State, r Request) bool {
	id := r.Member
	for depth := 0; depth <= len(s.Members); depth++ {
		m, ok := s.Members[id]
		if !ok || m.Launch != r.Launch {
			return false
		}
		if m.Parent == "" {
			return true
		}
		id = m.Parent
	}
	return false
}

// This model owns no SQL, implementation state, or transaction-order oracle.
// Crash effects occur within the original invocation/death interval. Recovery
// supplies a possible generated ID, never evidence that the original committed.
func specification() porcupine.Model {
	nm := porcupine.NondeterministicModel{
		Init:  func() []any { return []any{initial()} },
		Equal: func(a, b any) bool { return a.(string) == b.(string) },
		Hash:  func(v any) uint64 { h := fnv.New64a(); _, _ = h.Write([]byte(v.(string))); return h.Sum64() },
		Step: func(state, input, output any) []any {
			old := state.(string)
			r := input.(Request)
			out := output.(Observed)
			if out.Kind == "known_abort" {
				return []any{old}
			}
			if out.Kind == "known_commit" || out.Kind == "unknown" {
				var states []any
				if out.Kind == "unknown" {
					states = append(states, old)
				}
				if out.Candidate != nil && out.Candidate.OK {
					if next, ok := step(old, r, *out.Candidate); ok {
						states = append(states, next)
					}
				}
				return states
			}
			if out.Kind != "response" {
				return nil
			}
			if next, ok := step(old, r, out.Response); ok {
				return []any{next}
			}
			return nil
		},
		DescribeOperation: func(i, o any) string { return canonical(i) + " → " + canonical(o) },
		DescribeState:     func(s any) string { return s.(string) },
	}
	return nm.ToModel()
}

func step(old string, r Request, out Response) (string, bool) {
	var s State
	if err := json.Unmarshal([]byte(old), &s); err != nil {
		panic(err)
	}
	reject := func(part string) (string, bool) {
		return old, out.Version == 1 && !out.OK && strings.Contains(out.Error, part)
	}
	accept := func(result object) (string, bool) {
		if out.Version != 1 || !out.OK || !same(result, out.Result) {
			return old, false
		}
		if mutation(method(r)) {
			s.Replays[r.OperationID] = Replay{canonical(r), result}
		}
		return canonical(s), true
	}
	m, c := method(r), r.Command
	if r.Version != 1 {
		return reject("unsupported protocol")
	}
	if m != "launch_start" && m != "bind" && !valid(s, r) {
		return reject("stale launch")
	}
	if mutation(m) {
		if previous, ok := s.Replays[r.OperationID]; ok {
			if previous.Request != canonical(r) {
				return reject("operation_id reused")
			}
			return old, out.OK && out.Version == 1 && same(previous.Result, out.Result)
		}
	}
	switch m {
	case "launch_start":
		id := str(out.Result["launch"])
		if id == "" {
			return old, false
		}
		if _, ok := s.Launches[id]; ok {
			return old, false
		}
		s.Ordinal++
		s.Launches[id] = Launch{str(c["provider"]), str(c["instance"]), s.Ordinal}
		return accept(object{"launch": id})
	case "bind":
		l, ok := s.Launches[r.Launch]
		if !ok {
			return old, false
		}
		session, agent, parentAgent := str(c["session"]), str(c["agent"]), str(c["parent_agent"])
		parent := ""
		if agent != "" {
			for id, m := range s.Members {
				if m.Provider == l.Provider && m.Instance == l.Instance && m.Session == session && m.Agent == parentAgent && m.Launch == r.Launch {
					parent = id
				}
			}
			if parent == "" {
				return reject("parent startup hook")
			}
			if !valid(s, Request{Member: parent, Launch: r.Launch}) {
				return reject("stale launch")
			}
		}
		id := ""
		for key, m := range s.Members {
			if m.Provider == l.Provider && m.Instance == l.Instance && m.Session == session && m.Agent == agent {
				id = key
				if m.Ordinal > l.Ordinal {
					return reject("stale startup hook")
				}
				if m.Parent != parent {
					return reject("cannot change parents")
				}
			}
		}
		if id == "" {
			id = str(out.Result["member"])
			if id == "" {
				return old, false
			}
			if _, ok := s.Members[id]; ok {
				return old, false
			}
			s.MemberOrder = append(s.MemberOrder, id)
			s.Mailboxes[id] = Mailbox{Messages: []object{}}
		}
		s.Members[id] = Member{l.Provider, l.Instance, session, agent, parent, r.Launch, l.Ordinal}
		return accept(object{"member": id, "launch": r.Launch, "parent": nullable(parent)})
	case "members":
		list := []object{}
		for _, id := range s.MemberOrder {
			m := s.Members[id]
			list = append(list, object{"member": id, "provider": m.Provider, "instance": m.Instance, "session": m.Session, "agent": m.Agent, "parent": nullable(m.Parent), "launch": m.Launch})
		}
		return accept(object{"members": list})
	case "send":
		to := str(c["to"])
		box, ok := s.Mailboxes[to]
		if !ok {
			return reject("unknown recipient")
		}
		id := str(out.Result["message"])
		if id == "" {
			return old, false
		}
		for _, b := range s.Mailboxes {
			for _, message := range b.Messages {
				if message["message"] == id {
					return old, false
				}
			}
		}
		n := len(box.Messages) + 1
		box.Messages = append(box.Messages, object{"message": id, "sequence": n, "from": r.Member, "body": c["body"]})
		s.Mailboxes[to] = box
		return accept(object{"message": id, "sequence": n})
	case "inbox":
		limit := 4
		if v, ok := c["limit"]; ok {
			limit = integer(v)
		}
		if limit < 1 || limit > 4 {
			return reject("inbox limit")
		}
		b := s.Mailboxes[r.Member]
		if b.End == 0 && len(b.Messages) > b.Acked {
			b.End = min(len(b.Messages), b.Acked+limit)
		}
		messages := []object{}
		for n := b.Acked; n < min(b.End, b.Acked+limit); n++ {
			messages = append(messages, b.Messages[n])
		}
		s.Mailboxes[r.Member] = b
		return accept(object{"messages": messages, "acked": b.Acked, "batch_end": b.End})
	case "ack":
		b := s.Mailboxes[r.Member]
		seq := 0
		for n, message := range b.Messages {
			if message["message"] == c["through"] {
				seq = n + 1
			}
		}
		if seq == 0 {
			return reject("does not belong")
		}
		if seq > b.Acked {
			if seq > b.End {
				return reject("beyond fetched prefix")
			}
			b.Acked = seq
			if seq == b.End {
				b.End = 0
			}
		}
		s.Mailboxes[r.Member] = b
		return accept(object{"acked": b.Acked})
	case "cell_read", "cell_cas":
		key := str(c["key"])
		cell := s.Cells[key]
		value := func() object { return object{"key": key, "revision": cell.Revision, "value": cell.Value} }
		if m == "cell_read" {
			return accept(value())
		}
		swapped := integer(c["expected"]) == cell.Revision
		if swapped {
			cell.Revision++
			cell.Value = c["value"]
			s.Cells[key] = cell
		}
		return accept(object{"swapped": swapped, "cell": value()})
	case "task_create":
		id := str(out.Result["task"])
		if id == "" {
			return old, false
		}
		if _, ok := s.Tasks[id]; ok {
			return old, false
		}
		if _, ok := s.Members[str(c["owner"])]; !ok {
			return reject("FOREIGN KEY")
		}
		s.Tasks[id] = Task{str(c["owner"]), str(c["description"]), nil}
		return accept(object{"task": id})
	case "task_read", "task_finish":
		id := str(c["task"])
		t, ok := s.Tasks[id]
		if !ok {
			return reject("unknown task")
		}
		if m == "task_finish" {
			if t.Owner != r.Member {
				return reject("only task owner")
			}
			if t.Result != nil && !same(t.Result, c["result"]) {
				return reject("terminal task result is immutable")
			}
			t.Result = c["result"]
			s.Tasks[id] = t
		}
		return accept(object{"task": id, "owner": t.Owner, "description": t.Description, "result": t.Result})
	default:
		panic(fmt.Sprintf("unmodeled method %q", m))
	}
}
