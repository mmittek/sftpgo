// Background tasks (fork addition): one manager runs the work behind previews, cache upkeep
// and share cleanup.
//
//   - one queue with three priorities, served by a small pool of workers, highest first:
//     share work (share pages, share listings, link previews, previews of a new or edited
//     share), then browsing (previews and thumbnails of the logged-in file browser), then
//     maintenance (cache sweeps, share cleanup, cache clearing), which runs only when nothing
//     else is waiting
//   - a task has a key: submitting a key that is queued or running joins that task, and a
//     queued task moves up when a higher priority asks for it
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
	taskWorkers     = 2
	taskQueueSize   = 1000 // per priority
	taskStatusEvery = 15 * time.Second
)

var (
	errTaskDropped  = errors.New("the task queue is full, the task was dropped")
	errTaskCoolDown = errors.New("the task ran recently")
	tasks           = newTaskManager(taskWorkers)
)

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
	mu       sync.Mutex
	ready    *sync.Cond
	queues   [priorityCount][]*task
	workers  int
	active   map[string]*task // queued or running, by key
	running  map[string]*task
	lastDone map[string]time.Time // for cooldowns
	stats    map[string]*taskKindStats
	periodic []*periodicTask
	once     sync.Once
}

func newTaskManager(workers int) *taskManager {
	m := &taskManager{
		workers:  workers,
		active:   make(map[string]*task),
		running:  make(map[string]*task),
		lastDone: make(map[string]time.Time),
		stats:    make(map[string]*taskKindStats),
	}
	m.ready = sync.NewCond(&m.mu)
	return m
}

func (m *taskManager) start() {
	m.once.Do(func() {
		for range m.workers {
			go m.work()
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
}

// submit queues a task, or returns the queued or running task with the same key (moved up
// when this priority is higher). A key that finished less than cooldown ago is not run again
func (m *taskManager) submit(kind, key string, priority taskPriority, cooldown, timeout time.Duration,
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
	t := &task{kind: kind, key: fullKey, priority: priority, timeout: timeout, fn: fn, queued: time.Now(),
		done: make(chan struct{}), cooldown: cooldown}
	m.active[fullKey] = t
	m.queues[priority] = append(m.queues[priority], t)
	m.ready.Signal()
	return t, nil
}

// run submits a task and waits for it, or for ctx: the task keeps running when the caller
// stops waiting, so the next caller finds the work done
func (m *taskManager) run(ctx context.Context, kind, key string, priority taskPriority, timeout time.Duration,
	fn func(context.Context) error,
) error {
	t, err := m.submit(kind, key, priority, 0, timeout, fn)
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
			if _, err := m.submit(kind, "periodic", priorityMaintenance, 0, timeout, fn); err != nil {
				logger.Warn(logSender, "", "task %s: not scheduled: %v", kind, err)
			}
			m.mu.Lock()
			p.next = time.Now().Add(interval)
			m.mu.Unlock()
		}
	}()
}

// next waits for the highest priority queued task
func (m *taskManager) next() *task {
	m.mu.Lock()
	defer m.mu.Unlock()
	for {
		for p := range priorityCount {
			if queue := m.queues[p]; len(queue) > 0 {
				t := queue[0]
				m.queues[p] = queue[1:]
				t.started = time.Now()
				m.running[t.key] = t
				return t
			}
		}
		m.ready.Wait()
	}
}

func (m *taskManager) work() {
	for {
		t := m.next()
		t.err = m.execute(t)
		elapsed := time.Since(t.started)

		m.mu.Lock()
		delete(m.running, t.key)
		delete(m.active, t.key)
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
	Key      string  `json:"key"`
	Priority string  `json:"priority"`
	Seconds  float64 `json:"seconds"`
	Waited   float64 `json:"waited_seconds"`
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
		status.Running = append(status.Running, runningTaskStatus{Key: t.key, Priority: t.priority.String(),
			Seconds: time.Since(t.started).Round(time.Second).Seconds(), Waited: t.started.Sub(t.queued).Round(time.Second).Seconds()})
	}
	sort.Slice(status.Running, func(i, j int) bool { return status.Running[i].Key < status.Running[j].Key })
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
