package server

import (
	"bytes"
	"database/sql/driver"
	"fmt"
	"log"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"modernc.org/sqlite"
)

type alertPersistenceGate struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

var alertPersistenceGates sync.Map
var alertPersistenceGateID atomic.Int64

func init() {
	// Register before any tests open SQLite connections; each trigger selects its
	// own gate so blocked persistence never depends on scheduling or a sleep.
	sqlite.MustRegisterScalarFunction("test_alert_persistence_gate", 1, func(_ *sqlite.FunctionContext, args []driver.Value) (driver.Value, error) {
		value, ok := alertPersistenceGates.Load(args[0].(int64))
		if !ok {
			return nil, fmt.Errorf("unknown alert persistence gate")
		}
		gate := value.(*alertPersistenceGate)
		gate.once.Do(func() { close(gate.entered) })
		<-gate.release
		return int64(0), nil
	})
}

type alertLogCapture struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (c *alertLogCapture) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.Write(p)
}

func (c *alertLogCapture) String() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.String()
}

func waitAlertOperation(t *testing.T, done <-chan struct{}, operation string) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("%s blocked", operation)
	}
}

func TestAlertPersistenceDoesNotBlockEngine(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(fmt.Sprintf("persistence_failure=%t", fail), func(t *testing.T) {
			gate := &alertPersistenceGate{entered: make(chan struct{}), release: make(chan struct{})}
			id := alertPersistenceGateID.Add(1)
			alertPersistenceGates.Store(id, gate)
			t.Cleanup(func() { alertPersistenceGates.Delete(id) })
			store := newTestStore(t)
			var releaseOnce sync.Once
			release := func() { releaseOnce.Do(func() { close(gate.release) }) }
			t.Cleanup(release)
			reject := ""
			if fail {
				reject = "SELECT RAISE(FAIL, 'alert persistence rejected');"
			}
			if _, err := store.db.Exec(fmt.Sprintf(`CREATE TRIGGER block_alert_insert BEFORE INSERT ON alerts BEGIN
				SELECT test_alert_persistence_gate(%d); %s END`, id, reject)); err != nil {
				t.Fatal(err)
			}

			var logs alertLogCapture
			previousOutput := log.Writer()
			log.SetOutput(&logs)
			t.Cleanup(func() { log.SetOutput(previousOutput) })
			hub := NewHub()
			events := hub.Subscribe()
			t.Cleanup(hub.Close)
			rule := AlertRule{ID: "keys", Scope: "global", Metric: "keys_in_cooldown", Threshold: 1, CooldownSec: 60, Severity: "warning"}
			// Keep the mandatory telemetry rules quiet after the intentional write
			// failure so only this rule's cooldown and delivery are under test.
			rules := []AlertRule{rule,
				{ID: "telemetry-errors", Metric: "telemetry_errors", Threshold: 1e9},
				{ID: "telemetry-queued", Metric: "telemetry_queued", Threshold: 1e9},
			}
			engine := NewAlertEngine(store, hub, rules)
			now := time.Now()
			evalDone := make(chan struct{})
			go func() {
				defer close(evalDone)
				engine.Eval(now, func() int { return 2 })
			}()
			t.Cleanup(func() {
				release()
				waitAlertOperation(t, evalDone, "initial Eval cleanup")
			})
			waitAlertOperation(t, gate.entered, "entering SQLite alert trigger")

			feedDone := make(chan struct{})
			go func() {
				engine.Feed(tickAgg{TS: now.UnixMilli(), Req: 7}, nil, nil)
				close(feedDone)
			}()
			waitAlertOperation(t, feedDone, "Feed during blocked persistence")
			if got := engine.agg.Sum(60).Req; got != 7 {
				t.Fatalf("Feed lost activity: got %d requests, want 7", got)
			}

			var recent []AlertEvent
			recentDone := make(chan struct{})
			go func() {
				recent = engine.Recent(10)
				close(recentDone)
			}()
			waitAlertOperation(t, recentDone, "Recent during blocked persistence")
			want := AlertEvent{TS: now.UnixMilli(), RuleID: "keys", Name: "keys_in_cooldown · global", Severity: "warning", Scope: "global", Metric: "keys_in_cooldown", Value: 2, Threshold: 1, Message: "keys_in_cooldown 2.00 keys ≥ 1.00 keys over global"}
			if !reflect.DeepEqual(recent, []AlertEvent{want}) {
				t.Fatalf("recent alert = %+v, want %+v", recent, want)
			}

			rules[0].Severity = "critical"
			setDone := make(chan struct{})
			go func() {
				engine.SetRules(rules)
				close(setDone)
			}()
			waitAlertOperation(t, setDone, "SetRules during blocked persistence")
			if got := engine.Rules()[0].Severity; got != "critical" {
				t.Fatalf("updated severity = %q, want critical", got)
			}

			var evaluations sync.WaitGroup
			for range 16 {
				evaluations.Add(1)
				go func() {
					defer evaluations.Done()
					engine.Eval(now.Add(time.Second), func() int { return 3 })
				}()
			}
			concurrentDone := make(chan struct{})
			go func() { evaluations.Wait(); close(concurrentDone) }()
			waitAlertOperation(t, concurrentDone, "concurrent Eval inside reserved cooldown")
			if got := engine.Recent(10); !reflect.DeepEqual(got, []AlertEvent{want}) {
				t.Fatalf("concurrent Eval duplicated or changed alert: %+v", got)
			}
			select {
			case event := <-events:
				t.Fatalf("published before persistence returned: %+v", event)
			default:
			}

			release()
			waitAlertOperation(t, evalDone, "Eval after persistence release")
			select {
			case event := <-events:
				if event["type"] != "alert" || event["alert"] != want {
					t.Fatalf("published event = %+v, want original alert %+v", event, want)
				}
			default:
				t.Fatal("alert not published after persistence returned")
			}
			stored := store.RecentAlerts(10)
			if fail {
				if len(stored) != 0 || store.Telemetry().WriteErrors != 1 {
					t.Fatalf("failed persistence: rows=%+v telemetry=%+v", stored, store.Telemetry())
				}
				if !strings.Contains(logs.String(), "[alerts] persist:") || !strings.Contains(logs.String(), "alert persistence rejected") {
					t.Fatalf("missing persistence error report: %s", logs.String())
				}
			} else if !reflect.DeepEqual(stored, []AlertEvent{want}) {
				t.Fatalf("persisted alert = %+v, want %+v", stored, want)
			}
			if !strings.Contains(logs.String(), "[alert] warning "+want.Message) {
				t.Fatalf("missing original alert log: %s", logs.String())
			}
			if _, err := store.db.Exec("DROP TRIGGER block_alert_insert"); err != nil {
				t.Fatal(err)
			}

			// A write failure must neither cancel the reserved cooldown nor wedge
			// later rule evaluation; the exact boundary re-arms the updated rule.
			engine.Eval(now.Add(59*time.Second), func() int { return 3 })
			if got := len(engine.Recent(10)); got != 1 {
				t.Fatalf("cooldown after persistence: %d alerts, want 1", got)
			}
			engine.Eval(now.Add(60*time.Second), func() int { return 3 })
			got := engine.Recent(10)
			if len(got) != 2 || got[0].Severity != "critical" || got[0].Value != 3 || got[1] != want {
				t.Fatalf("re-armed rule lost updates or recent ordering: %+v", got)
			}
			if stored := store.RecentAlerts(10); len(stored) == 0 || stored[0] != got[0] {
				t.Fatalf("re-armed alert not persisted: %+v", stored)
			}
			select {
			case event := <-events:
				if event["alert"] != got[0] {
					t.Fatalf("re-armed event = %+v, want %+v", event, got[0])
				}
			default:
				t.Fatal("re-armed alert not published")
			}
			select {
			case event := <-events:
				t.Fatalf("duplicate alert publication: %+v", event)
			default:
			}
		})
	}
}
