package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/anishathalye/porcupine"
)

type History struct {
	Index                     int     `json:"index"`
	Seed                      int64   `json:"seed"`
	Concurrency               int     `json:"concurrency"`
	Result                    string  `json:"result"`
	Error                     string  `json:"error,omitempty"`
	CheckMS                   float64 `json:"checker_ms"`
	RunMS                     float64 `json:"run_ms"`
	OverlappingPairs          int     `json:"workload_overlapping_pairs"`
	SubmittedOverlappingPairs int     `json:"workload_submitted_overlapping_pairs"`
	Peak                      int     `json:"workload_peak_inflight"`
	Calls                     []*Call `json:"calls"`
}
type Summary struct {
	Started                   string            `json:"started_utc"`
	Finished                  string            `json:"finished_utc"`
	Binary                    string            `json:"binary"`
	BinarySHA256              string            `json:"binary_sha256"`
	Version                   object            `json:"binary_version"`
	GoVersion                 string            `json:"go_version"`
	GOOS                      string            `json:"goos"`
	GOARCH                    string            `json:"goarch"`
	Checker                   string            `json:"checker"`
	SourceSHA256              map[string]string `json:"verifier_source_sha256"`
	HistoriesSHA256           string            `json:"histories_sha256"`
	Seed                      int64             `json:"seed"`
	StartIndex                int               `json:"start_index"`
	RequestedHistories        int               `json:"requested_histories"`
	RequestedWorkloadCalls    int               `json:"requested_workload_calls_per_history"`
	Jobs                      int               `json:"history_jobs"`
	CompletedHistories        int               `json:"completed_histories"`
	Outcomes                  map[string]int    `json:"outcomes"`
	Strata                    map[string]int    `json:"concurrency_strata"`
	PhaseCalls                map[string]int    `json:"phase_calls"`
	RouteCalls                map[string]int    `json:"route_calls"`
	MethodCalls               map[string]int    `json:"method_calls"`
	MethodRouteCalls          map[string]int    `json:"method_route_calls"`
	ResponseKinds             map[string]int    `json:"response_kinds"`
	FaultConcurrency          map[string]int    `json:"fault_by_concurrency"`
	FaultOutcomes             map[string]int    `json:"fault_outcomes"`
	PublicErrorClasses        map[string]int    `json:"public_error_classes"`
	OverlappingPairs          int               `json:"workload_overlapping_pairs"`
	SubmittedOverlappingPairs int               `json:"workload_submitted_overlapping_pairs"`
	HistoriesWithOverlap      int               `json:"histories_with_workload_overlap"`
	Peak                      int               `json:"maximum_workload_inflight"`
	CheckerMS                 object            `json:"checker_ms"`
	WallSeconds               float64           `json:"wall_seconds"`
	Limitations               []string          `json:"limitations"`
}

func hashBytes(b []byte) string { sum := sha256.Sum256(b); return hex.EncodeToString(sum[:]) }
func hashFile(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		panic(err)
	}
	return hashBytes(b)
}
func percentiles(values []float64) object {
	if len(values) == 0 {
		return object{}
	}
	sort.Float64s(values)
	return object{"min": values[0], "p50": values[len(values)/2], "p95": values[min(len(values)-1, len(values)*95/100)], "max": values[len(values)-1]}
}
func overlap(calls []*Call) (int, int, int) {
	list := []*Call{}
	for _, c := range calls {
		if c.Phase == "workload" {
			list = append(list, c)
		}
	}
	pairs, submitted, peak := 0, 0, 0
	for i, a := range list {
		active := 0
		for j, b := range list {
			if a.Invoke >= b.Invoke && a.Invoke < b.Return {
				active++
			}
			if j > i && a.Invoke < b.Return && b.Invoke < a.Return {
				pairs++
			}
			if j > i && a.Submitted > 0 && b.Submitted > 0 && a.Submitted < b.Return && b.Submitted < a.Return {
				submitted++
			}
		}
		peak = max(peak, active)
	}
	return pairs, submitted, peak
}
func operations(calls []*Call) []porcupine.Operation {
	ops := make([]porcupine.Operation, len(calls))
	for i, c := range calls {
		ops[i] = porcupine.Operation{ClientId: c.Worker, Input: c.Request, Call: c.Invoke, Output: c.Observed, Return: c.Return}
	}
	return ops
}
func errorClass(s string) string {
	for _, part := range []string{"operation_id reused", "stale launch", "only task owner", "terminal task result", "does not belong", "beyond fetched prefix"} {
		if strings.Contains(s, part) {
			return part
		}
	}
	return s
}
func execute() int {
	binary := flag.String("binary", "../../target/debug/icarus-sync", "helper binary")
	count := flag.Int("histories", 1000, "number of independently seeded histories")
	calls := flag.Int("calls", 50, "workload calls per history (minimum 32; setup/audits separate)")
	seed := flag.Int64("seed", 20261014, "first history seed; history i uses seed+i")
	startIndex := flag.Int("start-index", 0, "first history index, preserving seed, fault case, and concurrency")
	output := flag.String("output", "../out/linear", "artifact directory")
	jobs := flag.Int("jobs", 1, "independent histories in parallel (1..16)")
	checkerTimeout := flag.Duration("checker-timeout", 5*time.Second, "per-history Porcupine timeout; Unknown fails the campaign")
	flag.Parse()
	if *count < 1 || *calls < 32 || *checkerTimeout <= 0 || *jobs < 1 || *jobs > 16 || *startIndex < 0 {
		fmt.Fprintln(os.Stderr, "histories must be positive, calls >=32, checker-timeout positive, jobs 1..16")
		return 2
	}
	var err error
	*binary, err = filepath.Abs(*binary)
	if err != nil {
		panic(err)
	}
	*output, err = filepath.Abs(*output)
	if err != nil {
		panic(err)
	}
	if err = os.MkdirAll(*output, 0755); err != nil {
		panic(err)
	}
	path := filepath.Join(*output, "histories.jsonl")
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0644)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	defer file.Close()
	encoder := json.NewEncoder(file)
	started := time.Now()
	versionBytes, err := exec.Command(*binary, "version").Output()
	if err != nil {
		panic(err)
	}
	var version object
	if err = json.Unmarshal(versionBytes, &version); err != nil {
		panic(err)
	}
	summary := Summary{Started: started.UTC().Format(time.RFC3339Nano), Binary: *binary, BinarySHA256: hashFile(*binary), Version: version, GoVersion: runtime.Version(), GOOS: runtime.GOOS, GOARCH: runtime.GOARCH, Checker: "github.com/anishathalye/porcupine v1.3.0", SourceSHA256: map[string]string{}, Seed: *seed, RequestedHistories: *count, RequestedWorkloadCalls: *calls, Jobs: *jobs, Outcomes: map[string]int{}, Strata: map[string]int{}, PhaseCalls: map[string]int{}, RouteCalls: map[string]int{}, MethodCalls: map[string]int{}, MethodRouteCalls: map[string]int{}, ResponseKinds: map[string]int{}, PublicErrorClasses: map[string]int{}, Limitations: []string{"Finite generated histories, not exhaustive schedule exploration or a proof.", "Intervals measure client invocation/return and request-submitted/return; they do not instrument SQLite lock overlap.", "Fault outcomes: known_abort is before_commit exit86; known_commit is after_commit exit86; unknown is SIGKILL without a protocol reply, bounded by observed process death.", "A public replay supplies possible generated IDs for crash transitions, not a commit oracle; prior public reads constrain effect-or-no-effect alternatives.", "No journal, SQL tables, or implementation transaction order are read by the checker.", "No native model calls, credentials, power-loss simulation, network filesystem, or liveness proof."}}
	for _, name := range []string{"go.mod", "go.sum", "model.go", "transport.go", "campaign.go", "main.go", "model_test.go"} {
		summary.SourceSHA256[name] = hashFile(name)
	}
	summary.StartIndex = *startIndex
	summary.FaultConcurrency = map[string]int{}
	summary.FaultOutcomes = map[string]int{}
	durations := []float64{}
	exit := 0
	indices := make(chan int)
	results := make(chan History, *jobs)
	var stopped atomic.Bool
	var workers sync.WaitGroup
	for worker := 0; worker < *jobs; worker++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for index := range indices {
				concurrency := []int{2, 4, 8}[index%3]
				start := time.Now()
				records, runErr := runHistory(*binary, *seed+int64(index), index, *calls, concurrency)
				h := History{Index: index, Seed: *seed + int64(index), Concurrency: concurrency, Calls: records, RunMS: float64(time.Since(start).Microseconds()) / 1000}
				h.OverlappingPairs, h.SubmittedOverlappingPairs, h.Peak = overlap(records)
				if runErr != nil {
					h.Result = "InfrastructureError"
					h.Error = runErr.Error()
				} else {
					checkStart := time.Now()
					result, info := porcupine.CheckOperationsVerbose(specification(), operations(records), *checkerTimeout)
					h.CheckMS = float64(time.Since(checkStart).Microseconds()) / 1000
					h.Result = string(result)
					if result != porcupine.Ok {
						visual := filepath.Join(*output, fmt.Sprintf("failure-seed-%d.html", h.Seed))
						if err := porcupine.VisualizePath(specification(), info, visual); err != nil {
							h.Error = "visualization: " + err.Error()
						}
					}
				}

				if h.Result != "Ok" {
					stopped.Store(true)
				}
				results <- h
			}
		}()
	}
	go func() {
		for index := *startIndex; index < *startIndex+*count && !stopped.Load(); index++ {
			indices <- index
		}
		close(indices)
		workers.Wait()
		close(results)
	}()
	pending := map[int]History{}
	next := *startIndex
	for received := range results {
		pending[received.Index] = received
		for {
			h, exists := pending[next]
			if !exists {
				break
			}
			delete(pending, next)
			next++
			if err = encoder.Encode(h); err != nil {
				panic(err)
			}
			if err = file.Sync(); err != nil {
				panic(err)
			}
			summary.CompletedHistories++
			summary.Outcomes[h.Result]++
			summary.Strata[fmt.Sprint(h.Concurrency)]++
			summary.OverlappingPairs += h.OverlappingPairs
			summary.SubmittedOverlappingPairs += h.SubmittedOverlappingPairs
			summary.Peak = max(summary.Peak, h.Peak)
			if h.OverlappingPairs > 0 {
				summary.HistoriesWithOverlap++
			}
			durations = append(durations, h.CheckMS)
			for _, c := range h.Calls {
				summary.PhaseCalls[c.Phase]++
				summary.RouteCalls[c.Route]++
				summary.MethodCalls[method(c.Request)]++
				summary.MethodRouteCalls[c.Route+":"+method(c.Request)]++
				summary.ResponseKinds[c.Observed.Kind]++
				if c.Fault != "" {
					summary.FaultConcurrency[fmt.Sprintf("%s/clients-%d", c.Fault, h.Concurrency)]++
					summary.FaultOutcomes[c.Fault+":"+c.Observed.Kind]++
				}
				if c.Observed.Kind == "response" && !c.Observed.Response.OK {
					summary.PublicErrorClasses[errorClass(c.Observed.Response.Error)]++
				}
			}
			if h.Result != "Ok" {
				exit = 1
				fmt.Fprintf(os.Stderr, "history %d seed %d: %s %s\n", h.Index, h.Seed, h.Result, h.Error)
			}
			if summary.CompletedHistories%25 == 0 || h.Result != "Ok" {
				fmt.Fprintf(os.Stderr, "checked %d/%d histories; outcomes=%v; elapsed=%s\n", summary.CompletedHistories, *count, summary.Outcomes, time.Since(started).Round(time.Second))
			}
		}
	}
	summary.Finished = time.Now().UTC().Format(time.RFC3339Nano)
	summary.WallSeconds = time.Since(started).Seconds()
	summary.CheckerMS = percentiles(durations)
	summary.HistoriesSHA256 = hashFile(path)
	encoded, err := json.MarshalIndent(summary, "", "  ")
	if err != nil {
		panic(err)
	}
	if err = os.WriteFile(filepath.Join(*output, "summary.json"), append(encoded, '\n'), 0644); err != nil {
		panic(err)
	}
	fmt.Println(string(encoded))
	return exit
}
func main() { os.Exit(execute()) }
