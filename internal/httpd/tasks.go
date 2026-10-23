// Background tasks (fork addition): one manager runs the work behind previews, cache upkeep
// and share cleanup.
//
//   - one queue with three priorities, served by a pool of workers running in parallel (4,
//     TASKS_WORKERS), highest first: share work (share pages, share listings, link previews,
//     previews of a new or edited share), then browsing (previews and thumbnails of the
//     logged-in file browser), then maintenance (cache sweeps, share cleanup, cache clearing)
//   - one worker is reserved for share work, so share pages and link previews never wait
//     behind the rest, and at most one maintenance task runs at a time
//   - a task has a key: submitting a key that is queued or running joins that task, and a
//     queued task moves up when a higher priority asks for it
//   - a task names the items it works on (files, or folders ending with "/"): two tasks never
//     run at the same time on overlapping items, a folder overlapping everything inside it; a
//     task held back this way stays queued and the worker takes the next one
//   - a key can have a cooldown: it is not run again before the cooldown has passed
//   - periodic tasks (maintenance) are registered in one place
//   - every task runs with a timeout and a panic guard; a full priority queue drops new work
//     instead of growing, it is submitted again on the next trigger
//   - a status file (TASKS_STATUS_FILE) is rewritten every 15 seconds with what is queued,
//     running and scheduled, per kind counters and timings

package httpd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/drakkan/sftpgo/v2/internal/logger"
)

type taskPriority int

const (
	priorityShare taskPriority = iota
	priorityBrowse
	priorityMaintenance
	priorityCount
)

func (p taskPriority) String() string {
	return [...]string{"share", "browse", "maintenance"}[p]
}

const (
	taskWorkersDefault = 4
	taskQueueSize      = 1000 // per priority
	taskStatusEvery    = 15 * time.Second
)

var (
	errTaskDropped  = errors.New("the task queue is full, the task was dropped")
	errTaskCoolDown = errors.New("the task ran recently")
	tasks           = newTaskManager(taskWorkersFromEnv())
)

func taskWorkersFromEnv() int {
	if n, err := strconv.Atoi(os.Getenv("TASKS_WORKERS")); err == nil && n >= 2 {
		return n
	}
	return taskWorkersDefault
}

type taskPriorityKey struct{}

// withTaskPriority marks the work a request asks for: share handlers use priorityShare, the
// logged-in file browser the default priorityBrowse
func withTaskPriority(ctx context.Context, p taskPriority) context.Context {
	return context.WithValue(ctx, taskPriorityKey{}, p)
}

func taskPriorityFrom(ctx context.Context) taskPriority {
	if p, ok := ctx.Value(taskPriorityKey{}).(taskPriority); ok {
		return p
	}
	return priorityBrowse
}

type task struct {
	kind     string
	key      string
	items    []string
	worker   int
	priority taskPriority
	timeout  time.Duration
	fn       func(context.Context) error
	queued   time.Time
	started  time.Time
	done     chan struct{}
	err      error
	cooldown time.Duration
}

type taskKindStats struct {
	Done       int64   `json:"done"`
	Failed     int64   `json:"failed"`
	Dropped    int64   `json:"dropped"`
	LastError  string  `json:"last_error,omitempty"`
	LastRun    string  `json:"last_run,omitempty"`
	AvgSeconds float64 `json:"avg_seconds"`
	totalTime  time.Duration
}

type periodicTask struct {
	kind     string
	interval time.Duration
	next     time.Time
}

type taskManager struct {
	mu      sync.Mutex
	ready   *sync.Cond
	queues  [priorityCount][]*task
	workers int
	// running tasks per priority, against limits: maintenance runs one at a time, browsing
	// leaves the reserved worker to share work
	runningBy [priorityCount]int
	limits    [priorityCount]int
	busy      []string         // items of the running tasks
	active    map[string]*task // queued or running, by key
	running   map[string]*task
	lastDone  map[string]time.Time // for cooldowns
	stats     map[string]*taskKindStats
	periodic  []*periodicTask
	once      sync.Once
}

func newTaskManager(workers int) *taskManager {
	workers = max(workers, 2)
	m := &taskManager{
		workers:  workers,
		active:   make(map[string]*task),
		running:  make(map[string]*task),
		lastDone: make(map[string]time.Time),
		stats:    make(map[string]*taskKindStats),
	}
	m.limits = [priorityCount]int{workers, workers - 1, 1}
	m.ready = sync.NewCond(&m.mu)
	return m
}

func (m *taskManager) start() {
	m.once.Do(func() {
		for worker := range m.workers {
			go m.work(worker + 1)
		}
		go m.writeStatus()
	})
}

func (m *taskManager) kindStats(kind string) *taskKindStats {
	s, ok := m.stats[kind]
	if !ok {
		s = &taskKindStats{}
		m.stats[kind] = s
	}
	return s
}

// promote moves a queued task to a higher priority queue
func (m *taskManager) promote(t *task, p taskPriority) {
	if t.priority <= p || !t.started.IsZero() {
		return
	}
	queue := m.queues[t.priority]
	for i, queued := range queue {
		if queued == t {
			m.queues[t.priority] = append(queue[:i:i], queue[i+1:]...)
			break
		}
	}
	t.priority = p
	m.queues[p] = append(m.queues[p], t)
	m.ready.Broadcast()
}

// submit queues a task, or returns the queued or running task with the same key (moved up
// when this priority is higher). A key that finished less than cooldown ago is not run again
func (m *taskManager) submit(kind, key string, items []string, priority taskPriority, cooldown, timeout time.Duration,
	fn func(context.Context) error,
) (*task, error) {
	m.start()
	fullKey := kind + ":" + key
	m.mu.Lock()
	defer m.mu.Unlock()
	if t, ok := m.active[fullKey]; ok {
		m.promote(t, priority)
		return t, nil
	}
	if cooldown > 0 && time.Since(m.lastDone[fullKey]) < cooldown {
		return nil, errTaskCoolDown
	}
	if len(m.queues[priority]) >= taskQueueSize {
		m.kindStats(kind).Dropped++
		return nil, errTaskDropped
	}
	t := &task{kind: kind, key: fullKey, items: items, priority: priority, timeout: timeout, fn: fn,
		queued: time.Now(), done: make(chan struct{}), cooldown: cooldown}
	m.active[fullKey] = t
	m.queues[priority] = append(m.queues[priority], t)
	m.ready.Broadcast()
	return t, nil
}

// run submits a task and waits for it, or for ctx: the task keeps running when the caller
// stops waiting, so the next caller finds the work done
func (m *taskManager) run(ctx context.Context, kind, key string, items []string, priority taskPriority,
	timeout time.Duration, fn func(context.Context) error,
) error {
	t, err := m.submit(kind, key, items, priority, 0, timeout, fn)
	if err != nil {
		return err
	}
	select {
	case <-t.done:
		return t.err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// every runs a maintenance task at a fixed interval, the first run one interval after start
func (m *taskManager) every(kind string, interval, timeout time.Duration, fn func(context.Context) error) {
	m.start()
	p := &periodicTask{kind: kind, interval: interval, next: time.Now().Add(interval)}
	m.mu.Lock()
	m.periodic = append(m.periodic, p)
	m.mu.Unlock()
	go func() {
		for {
			m.mu.Lock()
			wait := time.Until(p.next)
			m.mu.Unlock()
			time.Sleep(wait)
			if _, err := m.submit(kind, "periodic", nil, priorityMaintenance, 0, timeout, fn); err != nil {
				logger.Warn(logSender, "", "task %s: not scheduled: %v", kind, err)
			}
			m.mu.Lock()
			p.next = time.Now().Add(interval)
			m.mu.Unlock()
		}
	}()
}

// itemsOverlap: the same item, or a folder (ending with "/") and anything inside it
func itemsOverlap(a, b string) bool {
	if a == b {
		return true
	}
	return (strings.HasSuffix(a, "/") && strings.HasPrefix(b, a)) || (strings.HasSuffix(b, "/") && strings.HasPrefix(a, b))
}

func (m *taskManager) itemsBusy(items []string) bool {
	for _, item := range items {
		for _, busy := range m.busy {
			if itemsOverlap(item, busy) {
				return true
			}
		}
	}
	return false
}

// next waits for the highest priority queued task that its priority limit lets run and whose
// items no running task holds
func (m *taskManager) next(worker int) *task {
	m.mu.Lock()
	defer m.mu.Unlock()
	for {
		// everything but share work together leaves the reserved worker free
		others := m.runningBy[priorityBrowse] + m.runningBy[priorityMaintenance]
		for p := range priorityCount {
			if p != priorityShare && m.workers > 1 && others >= m.workers-1 {
				continue
			}
			if m.runningBy[p] >= m.limits[p] {
				continue
			}
			for i, t := range m.queues[p] {
				if m.itemsBusy(t.items) {
					continue
				}
				m.queues[p] = append(m.queues[p][:i:i], m.queues[p][i+1:]...)
				t.started = time.Now()
				t.worker = worker
				m.running[t.key] = t
				m.runningBy[p]++
				m.busy = append(m.busy, t.items...)
				return t
			}
		}
		m.ready.Wait()
	}
}

func (m *taskManager) release(t *task) {
	for _, item := range t.items {
		for i, busy := range m.busy {
			if busy == item {
				m.busy = append(m.busy[:i:i], m.busy[i+1:]...)
				break
			}
		}
	}
}

func (m *taskManager) work(worker int) {
	for {
		t := m.next(worker)
		t.err = m.execute(t)
		elapsed := time.Since(t.started)

		m.mu.Lock()
		delete(m.running, t.key)
		delete(m.active, t.key)
		m.runningBy[t.priority]--
		m.release(t)
		// a finished task may let a task held back by a priority limit run
		m.ready.Broadcast()
		if t.cooldown > 0 {
			m.lastDone[t.key] = time.Now()
			if len(m.lastDone) > 10000 {
				for k, done := range m.lastDone {
					if time.Since(done) > time.Hour {
						delete(m.lastDone, k)
					}
				}
			}
		}
		s := m.kindStats(t.kind)
		s.LastRun = time.Now().UTC().Format(time.RFC3339)
		s.totalTime += elapsed
		if t.err != nil {
			s.Failed++
			s.LastError = t.err.Error()
		} else {
			s.Done++
		}
		s.AvgSeconds = (s.totalTime / time.Duration(s.Done+s.Failed)).Seconds()
		m.mu.Unlock()
		close(t.done)
		if t.err != nil {
			logger.Warn(logSender, "", "task %s failed after %s: %v", t.key, elapsed.Round(time.Millisecond), t.err)
		}
	}
}

// execute runs a task with its timeout, a panic in the task fails it instead of the server
func (m *taskManager) execute(t *task) (err error) {
	ctx, cancel := context.WithTimeout(context.Background(), t.timeout)
	defer cancel()
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic: %v", r)
			logger.Error(logSender, "", "task %s panicked: %v\n%s", t.key, r, debug.Stack())
		}
	}()
	return t.fn(withTaskPriority(ctx, t.priority))
}

type taskStatus struct {
	Updated  string                    `json:"updated"`
	Workers  int                       `json:"workers"`
	Queued   map[string]int            `json:"queued"`
	Running  []runningTaskStatus       `json:"running"`
	Periodic []periodicTaskStatus      `json:"periodic"`
	Kinds    map[string]*taskKindStats `json:"kinds"`
}

type runningTaskStatus struct {
	Worker   int      `json:"worker"`
	Key      string   `json:"key"`
	Items    []string `json:"items,omitempty"`
	Priority string   `json:"priority"`
	Seconds  float64  `json:"seconds"`
	Waited   float64  `json:"waited_seconds"`
}

type periodicTaskStatus struct {
	Kind     string `json:"kind"`
	Interval string `json:"interval"`
	Next     string `json:"next"`
}

func (m *taskManager) snapshot() taskStatus {
	m.mu.Lock()
	defer m.mu.Unlock()
	status := taskStatus{Updated: time.Now().UTC().Format(time.RFC3339), Workers: m.workers,
		Queued: map[string]int{}, Kinds: map[string]*taskKindStats{}}
	for p := range priorityCount {
		status.Queued[p.String()] = len(m.queues[p])
	}
	for _, t := range m.running {
		status.Running = append(status.Running, runningTaskStatus{Worker: t.worker, Key: t.key, Items: t.items,
			Priority: t.priority.String(),
			Seconds:  time.Since(t.started).Round(time.Second).Seconds(), Waited: t.started.Sub(t.queued).Round(time.Second).Seconds()})
	}
	sort.Slice(status.Running, func(i, j int) bool { return status.Running[i].Worker < status.Running[j].Worker })
	for _, p := range m.periodic {
		status.Periodic = append(status.Periodic, periodicTaskStatus{Kind: p.kind, Interval: p.interval.String(),
			Next: p.next.UTC().Format(time.RFC3339)})
	}
	for kind, s := range m.stats {
		c := *s
		status.Kinds[kind] = &c
	}
	return status
}

// writeStatus rewrites the status file in place of growing a log: one small file, always current
func (m *taskManager) writeStatus() {
	file := os.Getenv("TASKS_STATUS_FILE")
	if file == "" {
		return
	}
	for {
		if data, err := json.MarshalIndent(m.snapshot(), "", "  "); err == nil {
			if err := writeFileAtomic(file, data); err != nil {
				logger.Debug(logSender, "", "unable to write the task status: %v", err)
			}
		}
		time.Sleep(taskStatusEvery)
	}
}
