package main

import (
	"fmt"
	"math/rand"
	"os"
	"sort"
	"sync"
	"time"
)

type Actor struct{ Member, Launch, Provider, Instance, Session, Agent string }
type runner struct {
	transport   transport
	clients     map[string]*mcpClient
	actors      []Actor
	calls       []*Call
	mu          sync.Mutex
	sequence    int
	random      *rand.Rand
	concurrency int
}

func (r *runner) request(actor int, command object) Request {
	r.sequence++
	q := Request{Version: 1, OperationID: fmt.Sprintf("op-%04d", r.sequence), Command: command}
	if actor >= 0 {
		q.Member = r.actors[actor].Member
		q.Launch = r.actors[actor].Launch
	}
	return q
}
func (r *runner) invoke(q Request, route, phase string, worker int, crash string, kill time.Duration) *Call {
	c := &Call{Request: q, Route: route, Phase: phase, Worker: worker, Invoke: r.transport.clock.stamp()}
	if crash != "" {
		c.Fault = crash
	} else if kill >= 0 {
		c.Fault = "sigkill"
		c.KillDelayUS = kill.Microseconds()
	}
	if route == "mcp" {
		c.Observed, c.Submitted = r.clients[q.Launch].call(q)
	} else {
		c.Observed, c.Submitted = r.transport.cli(q, crash, kill)
	}
	c.Return = r.transport.clock.stamp()
	r.mu.Lock()
	c.Index = len(r.calls)
	r.calls = append(r.calls, c)
	r.mu.Unlock()
	return c
}
func (r *runner) one(q Request, route, phase string) *Call {
	return r.invoke(q, route, phase, 0, "", -1)
}
func mustOK(c *Call) object {
	if c.Observed.Kind != "response" || !c.Observed.Response.OK {
		panic(fmt.Sprintf("unexpected operation failure: %s", canonical(c)))
	}
	return c.Observed.Response.Result
}
func (r *runner) addClient(launch string) {
	if r.clients[launch] != nil {
		return
	}
	c, err := r.transport.mcp(launch)
	if err != nil {
		panic(err)
	}
	r.clients[launch] = c
}
func (r *runner) route(n int) string {
	if (n+int(r.random.Int63()&1))%2 == 0 {
		return "cli"
	}
	return "mcp"
}
func (r *runner) batch(requests []Request, phase string) []*Call {
	outputs := make([]*Call, len(requests))
	routes := make([]string, len(requests))
	for i := range routes {
		routes[i] = r.route(i)
	}
	for first := 0; first < len(requests); first += r.concurrency {
		last := min(first+r.concurrency, len(requests))
		var wg sync.WaitGroup
		gate := make(chan struct{})
		for i := first; i < last; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				<-gate
				outputs[i] = r.invoke(requests[i], routes[i], phase, i-first, "", -1)
			}(i)
		}
		close(gate)
		wg.Wait()
	}
	return outputs
}
func (r *runner) setup() {
	for n, provider := range []string{"codex", "claude", "codex"} {
		a := Actor{Provider: provider, Instance: fmt.Sprintf("instance-%d", n), Session: fmt.Sprintf("session-%d", n)}
		a.Launch = str(mustOK(r.one(r.request(-1, object{"method": "launch_start", "provider": provider, "instance": a.Instance}), "cli", "setup"))["launch"])
		q := r.request(-1, object{"method": "bind", "session": a.Session, "agent": nil, "parent_agent": nil})
		q.Launch = a.Launch
		a.Member = str(mustOK(r.one(q, "cli", "setup"))["member"])
		r.actors = append(r.actors, a)
		r.addClient(a.Launch)
	}
	for _, parent := range []int{0, 1} {
		a := r.actors[parent]
		a.Agent = "child"
		q := r.request(parent, object{"method": "bind", "session": a.Session, "agent": a.Agent, "parent_agent": nil})
		a.Member = str(mustOK(r.one(q, "cli", "setup"))["member"])
		r.actors = append(r.actors, a)
	}
}
func (r *runner) workload(count int) []string {
	tasks := []string{}
	for owner := 0; owner < 2; owner++ {
		route := []string{"cli", "mcp"}[owner]
		c := r.one(r.request(owner, object{"method": "task_create", "owner": r.actors[owner].Member, "description": fmt.Sprintf("task-%d", owner)}), route, "workload")
		tasks = append(tasks, str(mustOK(c)["task"]))
	}
	sends := []Request{}
	for n := 0; n < 8; n++ {
		to := 1
		if n >= 4 {
			to = []int{0, 3}[n%2]
		}
		sends = append(sends, r.request(n%5, object{"method": "send", "to": r.actors[to].Member, "body": fmt.Sprintf("seed-%d", r.random.Int63())}))
	}
	r.batch(sends, "workload")
	cases := []Request{}
	for n := 0; n < 8; n++ {
		key := "cell-0"
		if n >= 6 {
			key = "cell-1"
		}
		cases = append(cases, r.request(n%5, object{"method": "cell_cas", "key": key, "expected": 0, "value": fmt.Sprintf("contender-%d", n)}))
	}
	r.batch(cases, "workload")
	inboxRequests := []Request{}
	for _, a := range []int{1, 0, 3} {
		inboxRequests = append(inboxRequests, r.request(a, object{"method": "inbox", "limit": 4}))
	}
	batches := r.batch(inboxRequests, "workload")
	first := decode[[]object](mustOK(batches[0])["messages"])
	if len(first) != 4 {
		panic("seeded mailbox did not contain four messages")
	}
	r.one(r.request(1, object{"method": "ack", "through": first[0]["message"]}), r.route(0), "workload")
	r.one(r.request(1, object{"method": "inbox", "limit": 2}), r.route(1), "workload")
	changed := decode[Request](sends[0])
	changed.Command["body"] = "changed arguments"
	cases = []Request{
		sends[0], sends[0], changed,
		r.request(0, object{"method": "ack", "through": first[1]["message"]}),
		r.request(1, object{"method": "task_finish", "task": tasks[0], "result": object{"status": "success", "value": "wrong owner"}}),
	}
	for owner := 0; owner < 2; owner++ {
		for _, value := range []string{"a", "b"} {
			cases = append(cases, r.request(owner, object{"method": "task_finish", "task": tasks[owner], "result": object{"status": "success", "value": value}}))
		}
	}
	r.random.Shuffle(len(cases), func(i, j int) { cases[i], cases[j] = cases[j], cases[i] })
	r.batch(cases, "workload")
	used := 32
	for used < count {
		width := min(r.concurrency, count-used)
		cases = nil
		for n := 0; n < width; n++ {
			a := r.random.Intn(5)
			var command object
			switch r.random.Intn(10) {
			case 0:
				command = object{"method": "members"}
			case 1:
				command = object{"method": "send", "to": r.actors[[]int{0, 1, 3}[r.random.Intn(3)]].Member, "body": fmt.Sprintf("random-%d", r.random.Int63())}
			case 2:
				command = object{"method": "inbox", "limit": 1 + r.random.Intn(4)}
			case 3:
				command = object{"method": "ack", "through": first[r.random.Intn(len(first))]["message"]}
				a = 1
			case 4:
				command = object{"method": "cell_read", "key": fmt.Sprintf("cell-%d", r.random.Intn(3))}
			case 5, 6:
				command = object{"method": "cell_cas", "key": fmt.Sprintf("cell-%d", r.random.Intn(3)), "expected": r.random.Intn(5), "value": r.random.Intn(10)}
			case 7:
				command = object{"method": "task_read", "task": tasks[r.random.Intn(2)]}
			case 8:
				cases = append(cases, sends[r.random.Intn(len(sends))])
				continue
			case 9:
				owner := r.random.Intn(2)
				a = owner
				command = object{"method": "task_finish", "task": tasks[owner], "result": object{"status": "success", "value": []string{"a", "b"}[r.random.Intn(2)]}}
			}
			cases = append(cases, r.request(a, command))
		}
		r.batch(cases, "workload")
		used += width
	}
	return tasks
}
func (r *runner) resume() {
	previous := r.actors[0]
	launch := str(mustOK(r.one(r.request(-1, object{"method": "launch_start", "provider": previous.Provider, "instance": previous.Instance}), "cli", "lifecycle"))["launch"])
	q := r.request(0, object{"method": "bind", "session": previous.Session, "agent": nil, "parent_agent": nil})
	q.Launch = launch
	main := mustOK(r.one(q, "cli", "lifecycle"))
	if main["member"] != previous.Member {
		panic("resume changed member ID")
	}
	r.one(r.request(0, object{"method": "members"}), "mcp", "lifecycle")
	r.one(r.request(3, object{"method": "members"}), "cli", "lifecycle")
	r.actors[0].Launch = launch
	r.addClient(launch)
	q = r.request(0, object{"method": "bind", "session": previous.Session, "agent": "child", "parent_agent": nil})
	child := mustOK(r.one(q, "cli", "lifecycle"))
	if child["member"] != r.actors[3].Member {
		panic("resume changed child ID")
	}
	r.actors[3].Launch = launch
}
func (r *runner) recover(history int) {
	q := r.request(2, object{"method": "send", "to": r.actors[2].Member, "body": "crash-window"})
	crash := ""
	kill := time.Duration(-1)
	switch (history / 3) % 3 {
	case 0:
		crash = "before_commit"
	case 1:
		crash = "after_commit"
	case 2:
		kill = time.Duration(r.random.Intn(5000)) * time.Microsecond
	}
	failed := r.invoke(q, "cli", "fault", 0, crash, kill)
	// This observation precedes replay. A retry that newly commits cannot
	// retroactively explain a message returned by this earlier public read.
	r.one(r.request(2, object{"method": "inbox", "limit": 4}), "mcp", "audit_before_replay")
	recovered := r.one(q, "mcp", "recovery")
	mustOK(recovered)
	candidate := recovered.Observed.Response
	failed.Observed.Candidate = &candidate
}
func (r *runner) audit(tasks []string) {
	for a := range r.actors {
		for page := 0; page < 100; page++ {
			c := r.one(r.request(a, object{"method": "inbox", "limit": 4}), r.route(a), "audit")
			messages := decode[[]object](mustOK(c)["messages"])
			if len(messages) == 0 {
				break
			}
			r.one(r.request(a, object{"method": "ack", "through": messages[len(messages)-1]["message"]}), r.route(page), "audit")
			if page == 99 {
				panic("mailbox audit did not terminate")
			}
		}
	}
	for n := 0; n < 3; n++ {
		r.one(r.request(0, object{"method": "cell_read", "key": fmt.Sprintf("cell-%d", n)}), r.route(n), "audit")
	}
	for n, task := range tasks {
		r.one(r.request(n, object{"method": "task_read", "task": task}), r.route(n), "audit")
	}
	r.one(r.request(0, object{"method": "members"}), "mcp", "audit")
}
func runHistory(binary string, seed int64, index, calls, concurrency int) (records []*Call, err error) {
	home, err := os.MkdirTemp("", "icarus-linear-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(home)
	r := &runner{transport: transport{binary: binary, home: home, group: "history", clock: &clock{start: time.Now()}}, clients: map[string]*mcpClient{}, random: rand.New(rand.NewSource(seed)), concurrency: concurrency}
	defer func() {
		for _, c := range r.clients {
			c.close()
		}
		sort.Slice(r.calls, func(i, j int) bool { return r.calls[i].Invoke < r.calls[j].Invoke })
		records = r.calls
		if p := recover(); p != nil {
			err = fmt.Errorf("%v", p)
		}
	}()
	r.setup()
	tasks := r.workload(calls)
	r.resume()
	r.recover(index)
	r.audit(tasks)
	return r.calls, nil
}
