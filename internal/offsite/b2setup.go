package offsite

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"time"

	"gitea.lan/matthew/fog/internal/objstore"
	"gitea.lan/matthew/fog/internal/randx"
)

// B2 setup: fog makes the bucket and the key itself from the operator's
// master application key, so a Backblaze target reaches grade A without
// the b2 command line. Backblaze's web console only offers keys that can
// delete what they wrote; its API can mint one that cannot. The master
// key is used for the length of one request — sign in, create the bucket,
// create a bucket-scoped key — and never stored, logged or audited.

// B2KeyCapabilities is what the key fog keeps may do: read, list, write,
// hide, and lock. No deleteFiles, no bucket or key management.
var B2KeyCapabilities = []string{
	"listBuckets", "readBuckets", "listFiles", "readFiles", "writeFiles",
	"readBucketRetentions", "readFileRetentions", "writeFileRetentions",
}

// b2AuthURL is Backblaze's sign-in endpoint; the Manager's B2AuthURL
// overrides it (tests).
const b2AuthURL = "https://api.backblazeb2.com"

var b2BucketRe = regexp.MustCompile(`^[a-zA-Z0-9-]{6,50}$`)

// B2SetupRequest is what the operator gives: a name for the target, the
// master key, and the bucket they want.
type B2SetupRequest struct {
	Name       string  `json:"name"`
	KeyID      string  `json:"keyId"`
	Secret     string  `json:"secret"`
	Bucket     string  `json:"bucket"`
	Prefix     string  `json:"prefix"`
	ObjectLock bool    `json:"objectLock"`
	PricePerTB float64 `json:"pricePerTb"`
}

// B2SetupReport says what fog did on Backblaze's side.
type B2SetupReport struct {
	Bucket string `json:"bucket"`
	// Adopted: the bucket already existed in this account (a retry after
	// a setup that got partway), so fog used it as it is.
	Adopted       bool     `json:"adopted"`
	ObjectLock    bool     `json:"objectLock"`
	LifecycleDays int      `json:"lifecycleDays"`
	KeyName       string   `json:"keyName"`
	Capabilities  []string `json:"capabilities"`
	// DeleteFallback: B2 refused to hide with the restricted key, so the
	// key fog kept also holds deleteFiles. Object Lock still protects
	// what fog writes; without it the target grades C.
	DeleteFallback bool `json:"deleteFallback,omitempty"`
}

// b2API is one signed-in session against the native API.
type b2API struct {
	hc        *http.Client
	accountID string
	apiURL    string
	s3URL     string
	token     string
}

// b2Error is the native API's error body.
type b2Error struct {
	Status  int    `json:"status"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *b2Error) Error() string {
	if e.Message != "" {
		return "Backblaze: " + e.Message + " (" + e.Code + ")"
	}
	return fmt.Sprintf("Backblaze answered %d %s", e.Status, e.Code)
}

func b2Authorize(ctx context.Context, hc *http.Client, base, keyID, secret string) (*b2API, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(base, "/")+"/b2api/v3/b2_authorize_account", nil)
	if err != nil {
		return nil, err
	}
	req.SetBasicAuth(keyID, secret)
	var out struct {
		AccountID string `json:"accountId"`
		Token     string `json:"authorizationToken"`
		APIInfo   struct {
			StorageAPI struct {
				APIURL   string `json:"apiUrl"`
				S3APIURL string `json:"s3ApiUrl"`
				// v3 names one bucket; v4 lists them. Either set means the
				// key is scoped to buckets and cannot make new ones.
				BucketID     *string  `json:"bucketId"`
				Buckets      []any    `json:"buckets"`
				Capabilities []string `json:"capabilities"`
			} `json:"storageApi"`
		} `json:"apiInfo"`
	}
	if err := b2Do(hc, req, &out); err != nil {
		var be *b2Error
		if errors.As(err, &be) && be.Status == http.StatusUnauthorized {
			return nil, errors.New("Backblaze did not accept that key id and application key")
		}
		return nil, err
	}
	sa := out.APIInfo.StorageAPI
	if sa.BucketID != nil || len(sa.Buckets) > 0 {
		return nil, errors.New("that key is restricted to a bucket, so it cannot create one — use the master application key (Application Keys → Master Application Key → Generate New Master Application Key), or a key for all buckets with writeBuckets and writeKeys")
	}
	var missing []string
	for _, need := range []string{"listBuckets", "writeBuckets", "writeKeys"} {
		if !slices.Contains(sa.Capabilities, need) {
			missing = append(missing, need)
		}
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("that key cannot set up a bucket (it lacks %s) — use the master application key, which is the only kind Backblaze's console makes that can create buckets and keys", strings.Join(missing, ", "))
	}
	if out.AccountID == "" || sa.APIURL == "" || sa.S3APIURL == "" {
		return nil, errors.New("Backblaze's sign-in answer was missing the account or API addresses")
	}
	return &b2API{hc: hc, accountID: out.AccountID, apiURL: strings.TrimRight(sa.APIURL, "/"), s3URL: strings.TrimRight(sa.S3APIURL, "/"), token: out.Token}, nil
}

func (a *b2API) call(ctx context.Context, op string, body, out any) error {
	b, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.apiURL+"/b2api/v3/"+op, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", a.token)
	req.Header.Set("Content-Type", "application/json")
	return b2Do(a.hc, req, out)
}

func b2Do(hc *http.Client, req *http.Request, out any) error {
	resp, err := hc.Do(req)
	if err != nil {
		return fmt.Errorf("reaching Backblaze: %w", err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		e := &b2Error{Status: resp.StatusCode}
		json.Unmarshal(data, e)
		e.Status = resp.StatusCode
		return e
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(data, out)
}

type b2Bucket struct {
	BucketID       string `json:"bucketId"`
	BucketName     string `json:"bucketName"`
	FileLockConfig struct {
		Value *struct {
			IsFileLockEnabled bool `json:"isFileLockEnabled"`
		} `json:"value"`
	} `json:"fileLockConfiguration"`
	LifecycleRules []struct {
		DaysFromHidingToDeleting *int   `json:"daysFromHidingToDeleting"`
		FileNamePrefix           string `json:"fileNamePrefix"`
	} `json:"lifecycleRules"`
}

func (b *b2Bucket) lockEnabled() bool {
	return b.FileLockConfig.Value != nil && b.FileLockConfig.Value.IsFileLockEnabled
}

// lifecycleDays is the whole-bucket hidden-file expiry, 0 when none.
func (b *b2Bucket) lifecycleDays() int {
	for _, r := range b.LifecycleRules {
		if r.FileNamePrefix == "" && r.DaysFromHidingToDeleting != nil {
			return *r.DaysFromHidingToDeleting
		}
	}
	return 0
}

func (a *b2API) createBucket(ctx context.Context, name string, lock bool, hiddenDays int) (*b2Bucket, error) {
	var out b2Bucket
	err := a.call(ctx, "b2_create_bucket", map[string]any{
		"accountId": a.accountID, "bucketName": name, "bucketType": "allPrivate", "fileLockEnabled": lock,
		"lifecycleRules": []map[string]any{{"fileNamePrefix": "", "daysFromHidingToDeleting": hiddenDays, "daysFromUploadingToHiding": nil}},
	}, &out)
	return &out, err
}

func (a *b2API) findBucket(ctx context.Context, name string) (*b2Bucket, error) {
	var out struct {
		Buckets []b2Bucket `json:"buckets"`
	}
	if err := a.call(ctx, "b2_list_buckets", map[string]any{"accountId": a.accountID, "bucketName": name}, &out); err != nil {
		return nil, err
	}
	for i := range out.Buckets {
		if out.Buckets[i].BucketName == name {
			return &out.Buckets[i], nil
		}
	}
	return nil, nil
}

func (a *b2API) deleteBucket(ctx context.Context, id string) error {
	return a.call(ctx, "b2_delete_bucket", map[string]any{"accountId": a.accountID, "bucketId": id}, nil)
}

type b2Key struct {
	ID     string `json:"applicationKeyId"`
	Secret string `json:"applicationKey"`
	Name   string `json:"keyName"`
}

func (a *b2API) createKey(ctx context.Context, name, bucketID string, caps []string) (*b2Key, error) {
	var out b2Key
	err := a.call(ctx, "b2_create_key", map[string]any{
		"accountId": a.accountID, "keyName": name, "bucketId": bucketID, "capabilities": caps,
	}, &out)
	if err == nil && (out.ID == "" || out.Secret == "") {
		err = errors.New("Backblaze created a key but did not return it")
	}
	return &out, err
}

func (a *b2API) deleteKey(ctx context.Context, id string) error {
	return a.call(ctx, "b2_delete_key", map[string]any{"applicationKeyId": id}, nil)
}

// b2Region is the part of the S3 endpoint between s3. and the domain.
func b2Region(s3URL string) string {
	h := strings.TrimPrefix(strings.TrimPrefix(s3URL, "https://"), "http://")
	h = strings.TrimPrefix(h, "s3.")
	if i := strings.Index(h, ".backblazeb2.com"); i > 0 {
		return h[:i]
	}
	return "us-west-004"
}

// B2HiddenDays is the bucket's hidden-file expiry fog sets: the lock span
// (recent tier plus a week), and never under two weeks. Until then a
// hidden file — fog's tidy-up, or an attacker's — can be brought back.
func B2HiddenDays(p Policy) int {
	if d := p.LockDays(); d > 14 {
		return d
	}
	return 14
}

// SetupB2 creates (or, on a retry, adopts) the bucket, mints the
// restricted key, and adds the target with it. The master key in req is
// not kept.
func (m *Manager) SetupB2(ctx context.Context, req B2SetupRequest) (*B2SetupReport, *ProbeResult, error) {
	req.Name = strings.TrimSpace(strings.ToLower(req.Name))
	req.Bucket = strings.TrimSpace(req.Bucket)
	req.KeyID, req.Secret = strings.TrimSpace(req.KeyID), strings.TrimSpace(req.Secret)
	if !nameRe.MatchString(req.Name) {
		return nil, nil, errors.New("name: lowercase letters, digits and dashes, up to 40 characters")
	}
	if !b2BucketRe.MatchString(req.Bucket) || strings.HasPrefix(strings.ToLower(req.Bucket), "b2-") {
		return nil, nil, errors.New("bucket: 6 to 50 letters, digits and dashes, not starting with b2- — and unique across all of Backblaze")
	}
	if req.KeyID == "" || req.Secret == "" {
		return nil, nil, errors.New("the master key id and application key are required")
	}
	if _, err := m.passphrase(); err != nil {
		return nil, nil, err
	}
	if _, err := m.Store.OffsiteTargetByName(req.Name); err == nil {
		return nil, nil, fmt.Errorf("a target named %s already exists", req.Name)
	}

	hc := m.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: time.Minute}
	}
	base := m.B2AuthURL
	if base == "" {
		base = b2AuthURL
	}
	api, err := b2Authorize(ctx, hc, base, req.KeyID, req.Secret)
	req.Secret = ""
	if err != nil {
		return nil, nil, err
	}

	rep := &B2SetupReport{Bucket: req.Bucket, Capabilities: B2KeyCapabilities}
	hidden := B2HiddenDays(ReadPolicy(m.Store))
	bucket, err := api.createBucket(ctx, req.Bucket, req.ObjectLock, hidden)
	created := err == nil
	if err != nil {
		var be *b2Error
		if !errors.As(err, &be) || be.Code != "duplicate_bucket_name" {
			return nil, nil, fmt.Errorf("creating the bucket: %w", err)
		}
		// Taken: ours from an earlier attempt, or someone else's.
		if bucket, err = api.findBucket(ctx, req.Bucket); err != nil {
			return nil, nil, fmt.Errorf("looking for the bucket: %w", err)
		}
		if bucket == nil {
			return nil, nil, fmt.Errorf("the bucket name %s is taken by another Backblaze account — bucket names are shared by everyone; choose another", req.Bucket)
		}
		rep.Adopted = true
		rep.ObjectLock = bucket.lockEnabled()
		rep.LifecycleDays = bucket.lifecycleDays()
	} else {
		rep.ObjectLock = req.ObjectLock
		rep.LifecycleDays = hidden
	}

	// From here a failure takes back what this call made: the keys, and
	// the bucket if Backblaze will still delete it (only while empty — a
	// probe's locked test object keeps it, and a retry adopts it).
	var keys []string
	undo := func() {
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		for _, id := range keys {
			api.deleteKey(cctx, id)
		}
		if created {
			api.deleteBucket(cctx, bucket.BucketID)
		}
	}

	rep.KeyName = "fog-" + req.Name + "-" + randx.Hex(3)
	key, err := api.createKey(ctx, rep.KeyName, bucket.BucketID, B2KeyCapabilities)
	if err != nil {
		undo()
		return nil, nil, fmt.Errorf("creating the application key: %w", err)
	}
	keys = append(keys, key.ID)

	spec := TargetSpec{Name: req.Name, Provider: "b2", Endpoint: api.s3URL, Region: b2Region(api.s3URL),
		Bucket: req.Bucket, Prefix: req.Prefix, PathStyle: true, PricePerTB: req.PricePerTB}
	if err := spec.normalize(); err != nil {
		undo()
		return nil, nil, err
	}

	// Probe the restricted key first. A new key can take a moment to
	// reach the S3 edge, hence the retries. If B2 will not let it hide —
	// S3 deletes may need deleteFiles there — swap in a key that has it:
	// fog's retention hides every night, and Object Lock is what keeps
	// that right from reaching locked history.
	probe, err := m.probeNewKey(ctx, spec, key)
	if err != nil {
		undo()
		return nil, probe, err
	}
	if !stepOK(probe, "hide") {
		caps := append(append([]string{}, B2KeyCapabilities...), "deleteFiles")
		wide, err := api.createKey(ctx, rep.KeyName+"-d", bucket.BucketID, caps)
		if err != nil {
			undo()
			return nil, probe, fmt.Errorf("creating the application key: %w", err)
		}
		keys = append(keys, wide.ID)
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		api.deleteKey(cctx, key.ID)
		cancel()
		keys = keys[1:]
		key = wide
		rep.KeyName, rep.Capabilities, rep.DeleteFallback = rep.KeyName+"-d", caps, true
		// The narrow key could not hide its test object; this one can.
		if c, err := m.clientFor(spec, key.ID, key.Secret); err == nil {
			scratch := Layout{Prefix: spec.Prefix, ID: "probe"}.ProbePrefix()
			c.List(ctx, scratch, func(o objstore.Object) error { return c.Delete(ctx, o.Key) })
		}
	}

	_, probe, err = m.AddTarget(ctx, spec, key.ID, key.Secret)
	if err != nil {
		undo()
		return nil, probe, err
	}
	return rep, probe, nil
}

// probeNewKey probes with a just-minted key, retrying while the key has
// not reached the S3 edge yet.
func (m *Manager) probeNewKey(ctx context.Context, spec TargetSpec, key *b2Key) (*ProbeResult, error) {
	c, err := m.clientFor(spec, key.ID, key.Secret)
	if err != nil {
		return nil, err
	}
	var probe *ProbeResult
	for attempt := 0; ; attempt++ {
		probe, err = Probe(ctx, c, Layout{Prefix: spec.Prefix, ID: "probe"}, IsB2(spec.Endpoint))
		if err == nil || attempt == 4 || !errors.Is(err, ErrProbeFailed) || len(probe.Steps) > 1 {
			return probe, err
		}
		select {
		case <-ctx.Done():
			return probe, err
		case <-time.After(m.b2Settle()):
		}
	}
}

func (m *Manager) b2Settle() time.Duration {
	if m.B2Settle > 0 {
		return m.B2Settle
	}
	return 3 * time.Second
}

func stepOK(p *ProbeResult, name string) bool {
	for _, s := range p.Steps {
		if s.Name == name {
			return s.OK
		}
	}
	return false
}
