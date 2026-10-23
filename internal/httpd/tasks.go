// Background tasks (fork addition): one manager runs the work behind previews, cache upkeep
// and share cleanup.
//
//   - two lanes: "interactive" for work someone is waiting for (a preview being viewed) and
//     "background" for upkeep (cache sweeps, share cleanup), each with its own workers, so
//     upkeep never delays a viewer and heavy disk work never runs twice at once
//   - a task has a key: submitting a key that is queued or running joins that task
//   - a key can have a cooldown: it is not run again before the cooldown has passed
//   - periodic tasks are registered in one place
//   - every task runs with a timeout and a panic guard; full queues drop upkeep work instead of
//     growing, it is submitted again on the next trigger
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

type taskLane string

const (
	laneInteractive taskLane = "interactive"
	laneBackground  taskLane = "background"
	taskQueueSize            = 1000
	taskStatusEvery          = 15 * time.Second
)

var (
	errTaskDropped  = errors.New("the task queue is full, the task was dropped")
	errTaskCoolDown = errors.New("the task ran recently")
	tasks           = newTaskManager(map[taskLane]int{laneInteractive: 2, laneBackground: 1})
)

type task struct {
	kind     string
	key      string
	lane     taskLane
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
	queues   map[taskLane]chan *task
	workers  map[taskLane]int
	active   map[string]*task // queued or running, by key
	running  map[string]*task
	lastDone map[string]time.Time // for cooldowns
	stats    map[string]*taskKindStats
	periodic []*periodicTask
	once     sync.Once
}

func newTaskManager(workers map[taskLane]int) *taskManager {
	m := &taskManager{
		queues:   make(map[taskLane]chan *task),
		workers:  workers,
		active:   make(map[string]*task),
		running:  make(map[string]*task),
		lastDone: make(map[string]time.Time),
		stats:    make(map[string]*taskKindStats),
	}
	for lane := range workers {
		m.queues[lane] = make(chan *task, taskQueueSize)
	}
	return m
}

func (m *taskManager) start() {
	m.once.Do(func() {
		for lane, n := range m.workers {
			for range n {
				go m.work(lane)
			}
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

// submit queues a task, or returns the queued or running task with the same key. A key that
// finished less than cooldown ago is not run again
func (m *taskManager) submit(kind, key string, lane taskLane, cooldown, timeout time.Duration,
	fn func(context.Context) error,
) (*task, error) {
	m.start()
	fullKey := kind + ":" + key
	m.mu.Lock()
	defer m.mu.Unlock()
	if t, ok := m.active[fullKey]; ok {
		return t, nil
	}
	if cooldown > 0 && time.Since(m.lastDone[fullKey]) < cooldown {
		return nil, errTaskCoolDown
	}
	t := &task{kind: kind, key: fullKey, lane: lane, timeout: timeout, fn: fn, queued: time.Now(),
		done: make(chan struct{}), cooldown: cooldown}
	select {
	case m.queues[lane] <- t:
		m.active[fullKey] = t
		return t, nil
	default:
		m.kindStats(kind).Dropped++
		return nil, errTaskDropped
	}
}

// run submits a task and waits for it, or for ctx: the task keeps running when the caller
// stops waiting, so the next caller finds the work done
func (m *taskManager) run(ctx context.Context, kind, key string, lane taskLane, timeout time.Duration,
	fn func(context.Context) error,
) error {
	t, err := m.submit(kind, key, lane, 0, timeout, fn)
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

// every runs a background task at a fixed interval, the first run one interval after start
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
			if _, err := m.submit(kind, "periodic", laneBackground, 0, timeout, fn); err != nil {
				logger.Warn(logSender, "", "task %s: not scheduled: %v", kind, err)
			}
			m.mu.Lock()
			p.next = time.Now().Add(interval)
			m.mu.Unlock()
		}
	}()
}

func (m *taskManager) work(lane taskLane) {
	for t := range m.queues[lane] {
		m.mu.Lock()
		t.started = time.Now()
		m.running[t.key] = t
		m.mu.Unlock()

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
	return t.fn(ctx)
}

type taskStatus struct {
	Updated  string                    `json:"updated"`
	Lanes    map[taskLane]laneStatus   `json:"lanes"`
	Running  []runningTaskStatus       `json:"running"`
	Periodic []periodicTaskStatus      `json:"periodic"`
	Kinds    map[string]*taskKindStats `json:"kinds"`
}

type laneStatus struct {
	Workers int `json:"workers"`
	Queued  int `json:"queued"`
}

type runningTaskStatus struct {
	Key     string  `json:"key"`
	Lane    string  `json:"lane"`
	Seconds float64 `json:"seconds"`
	Waited  float64 `json:"waited_seconds"`
}

type periodicTaskStatus struct {
	Kind     string `json:"kind"`
	Interval string `json:"interval"`
	Next     string `json:"next"`
}

func (m *taskManager) snapshot() taskStatus {
	m.mu.Lock()
	defer m.mu.Unlock()
	status := taskStatus{Updated: time.Now().UTC().Format(time.RFC3339), Lanes: map[taskLane]laneStatus{},
		Kinds: map[string]*taskKindStats{}}
	for lane, n := range m.workers {
		status.Lanes[lane] = laneStatus{Workers: n, Queued: len(m.queues[lane])}
	}
	for _, t := range m.running {
		status.Running = append(status.Running, runningTaskStatus{Key: t.key, Lane: string(t.lane),
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
