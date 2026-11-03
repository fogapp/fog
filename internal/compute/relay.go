package compute

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"path"
	"slices"
	"strings"
	"sync"
	"time"

	"gitea.lan/matthew/fog/internal/randx"
	"gitea.lan/matthew/fog/internal/store"
)

// Timing of the relay.
const (
	// reconcileEvery is how often the relay matches its pollers to the
	// pools and apps there are.
	reconcileEvery = time.Second
	// confirmWithin is how long a run handed to a machine may go
	// unmentioned in its reports before it counts as lost.
	confirmWithin = 15 * time.Second
	// defaultPoll is the app poll interval when the app names none.
	defaultPoll = 250 * time.Millisecond
	// failedPoll is the pause after an app poll that failed.
	failedPoll = 2 * time.Second
	// endpointTTL is how long an app's resolved address is reused: a poll
	// several times a second mustn't ask Docker each time.
	endpointTTL = 30 * time.Second
)

// ErrGone is an app that no longer knows a transcoder fog polls (404):
// fog forgets its id and introduces the pool again.
var ErrGone = errors.New("the app no longer knows this transcoder")

// Refusal reads an app's no (appcall.Refusal): a 404 is ErrGone.
func Refusal(resp *http.Response, _ []byte) error {
	if resp.StatusCode == http.StatusNotFound {
		return ErrGone
	}
	return fmt.Errorf("app answered %s", resp.Status)
}

// Caller makes one fog-compute/1 call to an app (appcall.Caller, reading
// refusals with Refusal).
type Caller interface {
	Call(ctx context.Context, app *store.App, method, path string, body any) ([]byte, error)
}

// Declared is what an app's fog.yaml says about compute.
type Declared struct {
	Contract string `json:"contract"`
	Video    *struct {
		Classes []string `json:"classes,omitempty"`
		Blurb   string   `json:"blurb,omitempty"`
	} `json:"video,omitempty"`
	Inference *struct {
		Blurb string `json:"blurb,omitempty"`
		// Locality "household" keeps the app's requests on machines that
		// report Local. Anything else it might say is treated the same: an
		// app that asked for a limit gets one, even if it misspelled it.
		Locality string `json:"locality,omitempty"`
	} `json:"inference,omitempty"`
}

// ParseDeclared reads an app's recorded declaration; nil when it makes
// none fog can honour.
func ParseDeclared(raw string) *Declared {
	if raw == "" {
		return nil
	}
	var d Declared
	if json.Unmarshal([]byte(raw), &d) != nil || d.Contract != Contract {
		return nil
	}
	return &d
}

// Relay is fogd as each pool's transcoder, to each app that declares
// video: it polls the app, places what it starts on machines with room,
// and reports back what they did.
type Relay struct {
	Service *Service
	Store   *store.Store
	Apps    Caller
	Log     *slog.Logger

	mu      sync.Mutex
	runs    map[string]*run
	pollers map[pollerKey]*poller
}

type pollerKey struct {
	app  int64
	pool string
}

// run is one run fog handed to a machine for an app.
type run struct {
	ID     string // fog's, as the machine knows it
	AppID  int64
	Pool   string
	AppRun string // the app's own id
	Node   string
	// Data is the run's token on the data route: the machine's whole
	// authority to reach the app, for as long as the run lives.
	Data      string
	Class     string
	Started   time.Time
	Confirmed bool
	// Stopping: the app asked for it to stop, so its exit is that.
	Stopping bool
	// activity is its row in the activity log.
	activity *tracked
	// done is closed when the run ends, however it ends: the data route's
	// requests for it end then too, even mid-transfer.
	done chan struct{}
}

// endLocked takes a run out of the relay: its data token stops working, and
// any request on the data route still using it is cut off.
func (r *Relay) endLocked(rn *run) {
	delete(r.runs, rn.ID)
	if rn.done != nil {
		select {
		case <-rn.done:
		default:
			close(rn.done)
		}
	}
}

// ending is how a run ended, recorded once the relay's lock is let go.
type ending struct {
	run     *run
	outcome string
	status  *int
	message string
}

// record finishes the activity rows of runs that ended. Called without
// r.mu: it writes to the database.
func (r *Relay) record(ends []ending) {
	for _, e := range ends {
		if e.run.activity != nil {
			r.Service.finish(e.run.activity, e.outcome, e.status, e.message)
		}
	}
}

// poller is one app's transcoder for one pool.
type poller struct {
	key    pollerKey
	app    store.App
	remote string
	cancel context.CancelFunc
	wake   chan struct{}

	// What the next poll reports, gathered from the machines' reports.
	exited   []Exited
	progress map[string]float64
	hello    string
	// batch: the app declares batch work, so it shares the pool's batch
	// slots; classes is every class it declared.
	batch   bool
	classes []string
}

// NewRelay wires a relay into its service, so machines' reports reach it.
func NewRelay(s *Service, st *store.Store, apps Caller, log *slog.Logger) *Relay {
	r := &Relay{Service: s, Store: st, Apps: apps, Log: log, runs: map[string]*run{}, pollers: map[pollerKey]*poller{}}
	s.relay = r
	// What the last fogd handed out it took with it.
	if n, err := st.LoseRunningComputeActivity(ActivityRestarted); err != nil {
		log.Warn("compute: activity unreadable", "err", err)
	} else if n > 0 {
		log.Info("compute: work in flight when fogd stopped is lost", "runs", n)
	}
	return r
}

// Run keeps the relay's pollers matched to pools and apps until ctx ends.
func (r *Relay) Run(ctx context.Context) {
	t := time.NewTicker(reconcileEvery)
	defer t.Stop()
	for {
		r.reconcile(ctx)
		select {
		case <-ctx.Done():
			r.mu.Lock()
			for _, p := range r.pollers {
				p.cancel()
			}
			r.mu.Unlock()
			return
		case <-t.C:
		}
	}
}

// videoApps answers the apps that declare video work, and the classes each
// declares (both, when it names none).
func (r *Relay) videoApps() ([]store.App, map[int64][]string, error) {
	all, err := r.Store.ListApps()
	if err != nil {
		return nil, nil, err
	}
	declared, err := r.Store.AllAppCompute()
	if err != nil {
		return nil, nil, err
	}
	var out []store.App
	classes := map[int64][]string{}
	for _, a := range all {
		d := ParseDeclared(declared[a.ID])
		if d == nil || d.Video == nil {
			continue
		}
		out = append(out, a)
		classes[a.ID] = d.Video.Classes
		if len(classes[a.ID]) == 0 {
			classes[a.ID] = []string{ClassInteractive, ClassBatch}
		}
	}
	return out, classes, nil
}

// reconcile starts a poller for every app × pool with a machine online,
// stops the rest, and loses the runs of machines gone quiet.
func (r *Relay) reconcile(ctx context.Context) {
	machines, err := r.Service.Registry.Machines()
	if err != nil {
		r.Log.Warn("compute: machines unreadable", "err", err)
		return
	}
	apps, declared, err := r.videoApps()
	if err != nil {
		r.Log.Warn("compute: apps unreadable", "err", err)
		return
	}
	// Asked of the registry as each run is judged, not of the snapshot
	// above: a run placed on a machine that paired since must not count
	// as lost.
	r.loseWhere(func(rn *run) string {
		switch {
		case !r.Service.Registry.online(rn.Node):
			return "the machine running it went offline"
		case !rn.Confirmed && time.Since(rn.Started) > confirmWithin:
			return "the machine never started it"
		}
		return ""
	})

	want := map[pollerKey]store.App{}
	for _, p := range Pools(machines) {
		for _, a := range apps {
			if !r.granted(a.ID, p.ID) {
				r.withdraw(ctx, a, p.ID)
				continue
			}
			if p.Online > 0 {
				want[pollerKey{a.ID, p.ID}] = a
			}
		}
	}
	r.mu.Lock()
	for k, p := range r.pollers {
		if _, ok := want[k]; !ok {
			p.cancel()
			delete(r.pollers, k)
		}
	}
	for k, a := range want {
		if p, ok := r.pollers[k]; ok {
			p.app = a
			continue
		}
		pctx, cancel := context.WithCancel(ctx)
		classes := declared[a.ID]
		p := &poller{key: k, app: a, cancel: cancel, wake: make(chan struct{}, 1), progress: map[string]float64{},
			classes: classes, batch: slices.Contains(classes, ClassBatch)}
		r.pollers[k] = p
		go r.poll(pctx, p)
	}
	r.mu.Unlock()
}

// granted reports whether the operator lets app use pool: every capable
// app gets every pool unless switched off in Settings › Compute.
func (r *Relay) granted(appID int64, pool string) bool {
	off, err := r.Store.ComputeGrantOff(appID, pool)
	return err == nil && !off
}

// withdraw takes a pool away from an app the operator switched off: the
// app forgets its transcoder, and the runs it had are lost.
func (r *Relay) withdraw(ctx context.Context, app store.App, pool string) {
	remote, err := r.Store.ComputeTranscoder(app.ID, pool)
	if err != nil {
		return
	}
	r.loseWhere(func(rn *run) string {
		if rn.AppID == app.ID && rn.Pool == pool {
			return "the operator took " + PoolName(pool) + " away from this app"
		}
		return ""
	})
	if _, err := r.Apps.Call(ctx, &app, "DELETE", AppTranscodersPath+"/"+remote, nil); err != nil && !errors.Is(err, ErrGone) {
		r.Log.Warn("compute: app didn't forget its transcoder", "app", app.Name, "pool", pool, "err", err)
		return
	}
	r.Store.DeleteComputeTranscoder(app.ID, pool)
	r.Log.Info("compute: pool taken away from app", "app", app.Name, "pool", pool)
}

// poll is one app's transcoder for one pool, until ctx ends.
func (r *Relay) poll(ctx context.Context, p *poller) {
	for ctx.Err() == nil {
		wait, err := r.pollOnce(ctx, p)
		if err != nil && ctx.Err() == nil {
			r.Log.Debug("compute: app poll failed", "app", p.app.Name, "pool", p.key.pool, "err", err)
			wait = failedPoll
		}
		select {
		case <-ctx.Done():
		case <-p.wake:
		case <-time.After(wait):
		}
	}
}

// transcoder answers the app's id for the pool's transcoder, introducing
// the pool to the app the first time.
func (r *Relay) transcoder(ctx context.Context, p *poller, pool Pool) (string, error) {
	if p.remote != "" {
		return p.remote, nil
	}
	id, err := r.Store.ComputeTranscoder(p.key.app, p.key.pool)
	if err == nil {
		p.remote = id
		return id, nil
	}
	if !errors.Is(err, store.ErrNotFound) {
		return "", err
	}
	raw, err := r.Apps.Call(ctx, &p.app, "POST", AppTranscodersPath,
		TranscoderRequest{Pool: pool.ID, Name: pool.Name, Hello: pool.Hello})
	if err != nil {
		return "", err
	}
	var ans TranscoderAnswer
	if err := json.Unmarshal(raw, &ans); err != nil || ans.ID == "" {
		return "", fmt.Errorf("%s answered no transcoder id", p.app.Name)
	}
	if err := r.Store.SetComputeTranscoder(p.key.app, p.key.pool, ans.ID); err != nil {
		return "", err
	}
	r.Log.Info("compute: app knows a fog transcoder", "app", p.app.Name, "pool", pool.ID, "id", ans.ID)
	p.remote = ans.ID
	return ans.ID, nil
}

// pollOnce is one poll of the app: what ran, what room there is; then the
// app's answer carried out. It answers how long until the next.
func (r *Relay) pollOnce(ctx context.Context, p *poller) (time.Duration, error) {
	machines, err := r.Service.Registry.Machines()
	if err != nil {
		return 0, err
	}
	var pool *Pool
	for _, pl := range Pools(machines) {
		if pl.ID == p.key.pool {
			pool = &pl
		}
	}
	if pool == nil || pool.Online == 0 {
		return failedPoll, nil
	}
	remote, err := r.transcoder(ctx, p, *pool)
	if err != nil {
		return 0, err
	}

	report, sent := r.report(p, machines, *pool)
	raw, err := r.Apps.Call(ctx, &p.app, "POST", AppTranscodersPath+"/"+remote+"/poll", report)
	if errors.Is(err, ErrGone) {
		// Its runs went with it; the next poll introduces the pool afresh.
		r.Log.Info("compute: app forgot its fog transcoder; introducing it again", "app", p.app.Name, "pool", p.key.pool)
		p.remote = ""
		r.mu.Lock()
		p.hello = ""
		r.mu.Unlock()
		r.loseWhere(func(rn *run) string {
			if rn.AppID == p.key.app && rn.Pool == p.key.pool {
				return "the app forgot the transcoder"
			}
			return ""
		})
		return 0, r.Store.DeleteComputeTranscoder(p.key.app, p.key.pool)
	}
	if err != nil {
		r.requeue(p, sent)
		return 0, err
	}
	var ans AppAnswer
	if err := json.Unmarshal(raw, &ans); err != nil {
		r.requeue(p, sent)
		return 0, fmt.Errorf("%s answered a poll fog can't read: %w", p.app.Name, err)
	}
	if report.Hello != nil {
		hello, _ := json.Marshal(report.Hello)
		r.mu.Lock()
		p.hello = string(hello)
		r.mu.Unlock()
	}
	for _, id := range ans.Stop {
		r.stop(p, id)
	}
	for _, start := range ans.Start {
		r.place(p, machines, start)
	}
	wait := defaultPoll
	if ans.PollMS > 0 {
		wait = time.Duration(ans.PollMS) * time.Millisecond
	}
	return wait, nil
}

// report gathers what the next poll says, taking the exits and progress it
// carries (handed back by requeue if the poll fails).
func (r *Relay) report(p *poller, machines []Machine, pool Pool) (AppReport, []Exited) {
	r.mu.Lock()
	defer r.mu.Unlock()
	rep := AppReport{Running: []string{}, Exited: p.exited, Progress: p.progress, Capacity: pool.Capacity}
	if rep.Exited == nil {
		rep.Exited = []Exited{}
	}
	p.exited, p.progress = nil, map[string]float64{}
	for _, rn := range r.runs {
		if rn.AppID == p.key.app && rn.Pool == p.key.pool {
			rep.Running = append(rep.Running, rn.AppRun)
		}
	}
	slices.Sort(rep.Running)
	rep.Free = r.freeLocked(machines, pool.ID, p.key.app)
	hello, _ := json.Marshal(pool.Hello)
	if string(hello) != p.hello {
		h := pool.Hello
		rep.Hello = &h
	}
	return rep, rep.Exited
}

// requeue hands back exits a poll failed to deliver.
func (r *Relay) requeue(p *poller, exited []Exited) {
	r.mu.Lock()
	defer r.mu.Unlock()
	p.exited = append(exited, p.exited...)
}

// A machine's slots: Interactive is how many runs it takes at once, and
// Batch how many of those may be batch work. A viewer's run may displace
// batch work, so a machine full of batch runs still has interactive room.

// usedLocked counts one machine's runs: all of them, and the batch ones.
func (r *Relay) usedLocked(node string) (all, batch int) {
	for _, rn := range r.runs {
		if rn.Node == node {
			all++
			if rn.Class == ClassBatch {
				batch++
			}
		}
	}
	return all, batch
}

// leftLocked is the room one machine has left, per class: interactive
// counting batch runs it could displace, batch only what's truly free.
func (r *Relay) leftLocked(m Machine) Slots {
	all, batch := r.usedLocked(m.ID)
	return Slots{
		Interactive: max(m.Slots.Interactive-(all-batch), 0),
		Batch:       max(min(m.Slots.Batch-batch, m.Slots.Interactive-all), 0),
	}
}

// freeLocked is the room the pool's online machines have left for one app:
// all of their interactive room, and the app's fair share of batch, so a
// long export from one app can't starve another's queue.
func (r *Relay) freeLocked(machines []Machine, pool string, app int64) Slots {
	var free, total Slots
	for _, m := range machines {
		if m.Pool != pool || !m.Online {
			continue
		}
		left := r.leftLocked(m)
		free.Interactive += left.Interactive
		free.Batch += left.Batch
		total.Batch += m.Slots.Batch
	}
	free.Batch = min(free.Batch, max(r.batchShareLocked(pool, total.Batch)-r.appBatchLocked(app, pool), 0))
	return free
}

// batchShareLocked is each app's fair share of a pool's batch slots: an
// even split among the apps fog polls for that pool that hand over batch
// work, rounded up.
func (r *Relay) batchShareLocked(pool string, total int) int {
	n := 0
	for k, p := range r.pollers {
		if k.pool == pool && p.batch {
			n++
		}
	}
	if n <= 1 {
		return total
	}
	return (total + n - 1) / n
}

// appBatchLocked counts an app's batch runs in a pool.
func (r *Relay) appBatchLocked(app int64, pool string) int {
	n := 0
	for _, rn := range r.runs {
		if rn.AppID == app && rn.Pool == pool && rn.Class == ClassBatch {
			n++
		}
	}
	return n
}

// shaped fills in what an app left to its kind: the class and outputs of
// the remote-transcoder protocol Stream spoke before fog-compute/1.
func shaped(a AppRun) (class string, outputs Outputs, err error) {
	class = a.Class
	switch {
	case class == ClassInteractive || class == ClassBatch:
	case a.Kind == "optimize":
		class = ClassBatch
	default:
		class = ClassInteractive
	}
	switch {
	case a.Outputs != nil:
		outputs = *a.Outputs
	case a.Kind == "session":
		outputs = Outputs{Init: []string{"init.mp4"}, Stream: "gop-*.m4s"}
	case a.Kind == "optimize":
		outputs = Outputs{Final: []string{"output.mp4"}}
	default:
		return "", Outputs{}, errors.New("the run names no outputs")
	}
	if a.Upload == "" {
		return "", Outputs{}, errors.New("the run has no upload URL")
	}
	// Uploads go where inputs come from: the app's /fog/compute/, through
	// the run's data route. (The machine checks this too.)
	if !strings.HasPrefix(a.Upload, "{server}"+AppDataPrefix) || strings.Contains(a.Upload, "..") {
		return "", Outputs{}, fmt.Errorf("the upload URL must be under {server}%s", AppDataPrefix)
	}
	if l := a.Limits; l != nil && (l.Seconds < 0 || l.ScratchBytes < 0) {
		return "", Outputs{}, errors.New("a run's limits can't be negative")
	}
	for _, name := range append(append([]string{outputs.Stream}, outputs.Init...), outputs.Final...) {
		if name != "" && (path.IsAbs(name) || path.Clean(name) != name || name == ".." || len(name) > 2 && name[:3] == "../") {
			return "", Outputs{}, fmt.Errorf("output %q is outside the run's directory", name)
		}
	}
	return class, outputs, nil
}

// place gives one run the app started to a machine in the pool with room,
// or reports it exited when none has.
func (r *Relay) place(p *poller, machines []Machine, a AppRun) {
	row := store.ComputeActivity{App: p.app.Name, Pool: p.key.pool, Kind: KindVideo, What: a.Class}
	refuse := func(why string) {
		r.mu.Lock()
		p.exited = append(p.exited, Exited{ID: a.ID, Log: "fog: " + why})
		r.mu.Unlock()
		r.Service.refused(context.Background(), row, nil, why)
	}
	class, outputs, err := shaped(a)
	if err != nil {
		refuse(err.Error())
		return
	}
	row.What = class
	if !slices.Contains(p.classes, class) {
		refuse("the app didn't declare " + class + " work")
		return
	}
	r.mu.Lock()
	if class == ClassBatch && r.appBatchLocked(p.key.app, p.key.pool) >= r.batchShareLocked(p.key.pool, poolBatch(machines, p.key.pool)) {
		r.mu.Unlock()
		refuse("this app has its share of " + PoolName(p.key.pool) + "'s batch room")
		return
	}
	best, displace := r.chooseLocked(machines, p.key.pool, class)
	if best == nil {
		r.mu.Unlock()
		refuse("no machine in " + PoolName(p.key.pool) + " had room for it")
		return
	}
	var ends []ending
	if displace != nil {
		// A viewer's run takes the place of batch work, which its app
		// makes again later.
		r.endLocked(displace)
		if dp := r.pollers[pollerKey{displace.AppID, displace.Pool}]; dp != nil {
			dp.exited = append(dp.exited, Exited{ID: displace.AppRun, Log: "fog: stopped to make room for a viewer"})
		}
		ends = append(ends, ending{displace, OutcomeDisplaced, nil, "stopped to make room for a viewer"})
		go r.Service.deliver(displace.Node, nil, []string{displace.ID})
	}
	rn := &run{ID: "r" + randx.Hex(8), AppID: p.key.app, Pool: p.key.pool, AppRun: a.ID, Node: best.ID, Class: class,
		Started: time.Now(), Data: randx.Token(), done: make(chan struct{})}
	row.Machine = best.Name
	rn.activity = r.Service.begin(context.Background(), row)
	r.runs[rn.ID] = rn
	r.mu.Unlock()
	r.record(ends)
	r.Service.deliver(best.ID, []NodeRun{{
		ID: rn.ID, Class: class, Argv: a.Argv, Server: DataPath + rn.Data, Outputs: outputs, Upload: a.Upload, Limits: a.Limits,
	}}, nil)
}

// chooseLocked picks the machine for a run of class: the least loaded
// with room, and for interactive work with none, one whose batch run it
// may displace (the newest, which loses least).
func (r *Relay) chooseLocked(machines []Machine, pool, class string) (*Machine, *run) {
	var best *Machine
	var bestLoad float64
	var fallback *Machine
	for i := range machines {
		m := &machines[i]
		if m.Pool != pool || !m.Online || m.Slots.Interactive <= 0 {
			continue
		}
		all, _ := r.usedLocked(m.ID)
		left := r.leftLocked(*m)
		room := left.Batch
		if class == ClassInteractive {
			room = m.Slots.Interactive - all
		}
		if room > 0 {
			load := float64(all) / float64(m.Slots.Interactive)
			if best == nil || load < bestLoad {
				best, bestLoad = m, load
			}
		} else if class == ClassInteractive && left.Interactive > 0 && fallback == nil {
			fallback = m
		}
	}
	if best != nil || fallback == nil {
		return best, nil
	}
	var newest *run
	for _, rn := range r.runs {
		if rn.Node == fallback.ID && rn.Class == ClassBatch && (newest == nil || rn.Started.After(newest.Started)) {
			newest = rn
		}
	}
	return fallback, newest
}

// poolBatch is all the batch slots of a pool's online machines.
func poolBatch(machines []Machine, pool string) int {
	n := 0
	for _, m := range machines {
		if m.Pool == pool && m.Online {
			n += m.Slots.Batch
		}
	}
	return n
}

// stop asks the machine running one of the app's runs to stop it.
func (r *Relay) stop(p *poller, appRun string) {
	r.mu.Lock()
	var target *run
	for _, rn := range r.runs {
		if rn.AppID == p.key.app && rn.Pool == p.key.pool && rn.AppRun == appRun {
			target = rn
			rn.Stopping = true
		}
	}
	r.mu.Unlock()
	if target != nil {
		r.Service.deliver(target.Node, nil, []string{target.ID})
	}
}

// fromNode takes in what a machine said about the runs it holds.
func (r *Relay) fromNode(node string, report NodeReport) {
	var ends []ending
	defer func() { r.record(ends) }()
	r.mu.Lock()
	defer r.mu.Unlock()
	woken := map[*poller]bool{}
	for _, id := range report.Running {
		if rn, ok := r.runs[id]; ok && rn.Node == node {
			rn.Confirmed = true
		}
	}
	for _, e := range report.Exited {
		rn, ok := r.runs[e.ID]
		if !ok || rn.Node != node {
			continue
		}
		r.endLocked(rn)
		if p := r.pollers[pollerKey{rn.AppID, rn.Pool}]; p != nil {
			p.exited = append(p.exited, Exited{ID: rn.AppRun, Status: e.Status, Log: e.Log})
			woken[p] = true
		}
		ends = append(ends, exitEnding(rn, e))
	}
	for id, seconds := range report.Progress {
		if rn, ok := r.runs[id]; ok && rn.Node == node {
			if p := r.pollers[pollerKey{rn.AppID, rn.Pool}]; p != nil {
				p.progress[rn.AppRun] = seconds
			}
		}
	}
	// A machine still running something fog no longer holds — stopped
	// while it was offline, or from before fogd restarted — is told to
	// stop it, rather than finishing work nobody will use.
	var orphans []string
	for _, id := range report.Running {
		if _, ok := r.runs[id]; !ok {
			orphans = append(orphans, id)
		}
	}
	if len(orphans) > 0 {
		go r.Service.deliver(node, nil, orphans)
	}
	for p := range woken {
		select {
		case p.wake <- struct{}{}:
		default:
		}
	}
}

// loseWhere reports lost every run why answers a reason for.
func (r *Relay) loseWhere(why func(*run) string) {
	var ends []ending
	defer func() { r.record(ends) }()
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, rn := range r.runs {
		reason := why(rn)
		if reason == "" {
			continue
		}
		r.endLocked(rn)
		if p := r.pollers[pollerKey{rn.AppID, rn.Pool}]; p != nil {
			p.exited = append(p.exited, Exited{ID: rn.AppRun, Log: "fog: " + reason})
		}
		ends = append(ends, ending{rn, OutcomeLost, nil, reason})
	}
}

// exitEnding reads a machine's report of a run's exit: ok on status 0,
// stopped when the app asked for it, failed otherwise with its log's tail.
func exitEnding(rn *run, e Exited) ending {
	switch {
	case rn.Stopping:
		return ending{rn, OutcomeStopped, e.Status, "the app stopped it"}
	case e.Status != nil && *e.Status == 0:
		return ending{rn, OutcomeOK, e.Status, ""}
	}
	return ending{rn, OutcomeFailed, e.Status, e.Log}
}

// loseNode reports lost every run on a removed machine.
func (r *Relay) loseNode(node, reason string) {
	r.loseWhere(func(rn *run) string {
		if rn.Node == node {
			return reason
		}
		return ""
	})
}

// Usage is one app's use of one pool, for Settings › Compute.
type Usage struct {
	App     string `json:"app"`
	Pool    string `json:"pool"`
	On      bool   `json:"on"`
	Polling bool   `json:"polling"`
	Running Slots  `json:"running"`
}

// Usage answers every app that declares video work against every pool:
// whether it's switched on, whether fog is polling it, and what it runs.
func (r *Relay) Usage(pools []Pool) ([]Usage, error) {
	apps, _, err := r.videoApps()
	if err != nil {
		return nil, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	out := []Usage{}
	for _, p := range pools {
		if p.ID == InferencePool {
			continue
		}
		for _, a := range apps {
			u := Usage{App: a.Name, Pool: p.ID, On: r.granted(a.ID, p.ID)}
			_, u.Polling = r.pollers[pollerKey{a.ID, p.ID}]
			for _, rn := range r.runs {
				if rn.AppID == a.ID && rn.Pool == p.ID {
					if rn.Class == ClassBatch {
						u.Running.Batch++
					} else {
						u.Running.Interactive++
					}
				}
			}
			out = append(out, u)
		}
	}
	return out, nil
}
