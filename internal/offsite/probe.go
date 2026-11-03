package offsite

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"gitea.lan/matthew/fog/internal/objstore"
	"gitea.lan/matthew/fog/internal/randx"
)

// The probe: what fog can actually do to this bucket with this key, and
// what it therefore cannot destroy. The same discipline as the NAS probe
// — one uniquely named object created and then hidden, nothing else
// touched — and the answer is a grade the operator can read.

// ProbeStep is one thing the probe tried.
type ProbeStep struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail,omitempty"`
}

// ProbeResult is the probe's verdict, stored on the target row as JSON.
type ProbeResult struct {
	At    string      `json:"at"`
	Grade string      `json:"grade" ts:"\"A\" | \"B\" | \"C\" | \"D\" | \"\""` // A | B | C | D
	Steps []ProbeStep `json:"steps"`
	// The facts the grade rests on.
	Versioning        string              `json:"versioning"`
	Lock              objstore.LockConfig `json:"lock"`
	CanLock           bool                `json:"canLock"`
	CanDeleteVersions bool                `json:"canDeleteVersions"`
	// LockHolds: a key that may delete versions was refused on a locked
	// one. Object Lock then protects what fog writes whatever the key's
	// capabilities — the only route to grade A where the provider's
	// console cannot make a key without the delete right (Backblaze).
	LockHolds bool `json:"lockHolds,omitempty"`
	// LockDenied: the locked test upload was refused for want of a right,
	// not for some other fault — only then is the key the fix.
	LockDenied bool `json:"lockDenied,omitempty"`
	// Fix is the one change that raises the grade; empty at A.
	Fix string `json:"fix,omitempty"`
	// Recommendations are worth doing at any grade (a lifecycle rule).
	Recommendations []string `json:"recommendations,omitempty"`
}

// GradeLabel is the operator-facing word for a grade.
func GradeLabel(g string) string {
	switch g {
	case "A":
		return "Immutable"
	case "B":
		return "Append-only"
	case "C":
		return "Versioned"
	case "D":
		return "Plain"
	}
	return "Unknown"
}

// ErrProbeFailed means the bucket could not be used at all — wrong
// credentials, wrong bucket, unreachable endpoint. The steps say which.
var ErrProbeFailed = errors.New("the bucket could not be used")

// Probe runs the checks against one target and grades it. isB2 shapes the
// fix text in the provider's own vocabulary.
func Probe(ctx context.Context, c *objstore.Client, layout Layout, isB2 bool) (*ProbeResult, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	res := &ProbeResult{At: time.Now().UTC().Format(time.RFC3339)}
	step := func(name string, ok bool, detail string) {
		res.Steps = append(res.Steps, ProbeStep{Name: name, OK: ok, Detail: detail})
	}
	key := layout.ProbePrefix() + randx.Hex(8)
	body := []byte("fog offsite probe " + res.At)

	// 1. Write.
	if _, err := c.PutBytes(ctx, key, body, objstore.PutOptions{ContentType: "text/plain"}); err != nil {
		step("write", false, err.Error())
		return res, fmt.Errorf("%w: writing a test object failed: %v", ErrProbeFailed, err)
	}
	step("write", true, "")
	// From here on the probe object exists; hide it whatever happens.
	defer func() {
		hctx, hcancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer hcancel()
		c.Delete(hctx, key)
	}()

	// 2. Read back.
	got, _, err := c.GetBytes(ctx, key, 1<<10)
	if err != nil || string(got) != string(body) {
		detail := "read back different bytes"
		if err != nil {
			detail = err.Error()
		}
		step("read", false, detail)
		return res, fmt.Errorf("%w: the key can write but not read: %s", ErrProbeFailed, detail)
	}
	step("read", true, "")

	// 3. Listing (restore-from-nothing needs it).
	found := false
	if err := c.List(ctx, layout.ProbePrefix(), func(o objstore.Object) error {
		if o.Key == key {
			found = true
		}
		return nil
	}); err != nil || !found {
		detail := "the test object was not listed"
		if err != nil {
			detail = err.Error()
		}
		step("list", false, detail)
		return res, fmt.Errorf("%w: the key cannot list the bucket: %s", ErrProbeFailed, detail)
	}
	step("list", true, "")

	// 4. Versioning.
	ver, err := c.Versioning(ctx)
	if err != nil {
		step("versioning", false, err.Error())
	} else {
		res.Versioning = ver
		step("versioning", ver == "Enabled", "bucket versioning: "+orNone(ver))
	}

	// 5. Hide, then try to destroy the version.
	if err := c.Delete(ctx, key); err != nil {
		step("hide", false, err.Error())
	} else {
		step("hide", true, "")
	}
	var dataVersion string
	c.ListVersions(ctx, key, func(v objstore.Version) error {
		if !v.DeleteMarker && v.Key == key {
			dataVersion = v.VersionID
		}
		return nil
	})
	switch {
	case res.Versioning != "Enabled":
		// Nothing to test: an unversioned delete already destroyed it.
		res.CanDeleteVersions = true
		step("version delete", false, "no versions to protect")
	case dataVersion == "":
		step("version delete", false, "the hidden object's version was not listed")
		res.CanDeleteVersions = true
	default:
		err := c.DeleteVersion(ctx, key, dataVersion)
		switch {
		case err == nil:
			res.CanDeleteVersions = true
			step("version delete", false, "the key CAN permanently delete versions")
		case errors.Is(err, objstore.ErrAccessDenied):
			step("version delete", true, "refused — the key cannot destroy data")
		default:
			step("version delete", false, err.Error())
			res.CanDeleteVersions = true
		}
	}

	// 6. Object Lock: is it on, and can this key lock what it writes?
	lock, err := c.ObjectLock(ctx)
	if err != nil {
		step("object lock", false, err.Error())
	} else {
		res.Lock = lock
		if lock.Enabled {
			lkey := key + "-locked"
			until := time.Now().Add(24 * time.Hour)
			if _, err := c.PutBytes(ctx, lkey, body, objstore.PutOptions{LockMode: "COMPLIANCE", RetainUntil: until}); err != nil {
				res.LockDenied = errors.Is(err, objstore.ErrAccessDenied)
				if res.LockDenied {
					step("object lock", false, "enabled, but the key cannot set retention: "+err.Error())
				} else {
					step("object lock", false, "enabled, but a locked test upload failed: "+err.Error())
				}
			} else {
				res.CanLock = true
				step("object lock", true, "enabled; a compliance lock was set on a test object (hidden, expires in a day)")
				c.Delete(ctx, lkey)
				if res.CanDeleteVersions {
					res.LockHolds = lockedDeleteRefused(ctx, c, lkey)
					if res.LockHolds {
						step("locked delete", true, "refused — the key can delete versions, but not locked ones, and fog locks everything it writes")
					} else {
						step("locked delete", false, "the key deleted a locked version — the lock did not hold")
					}
				}
			}
		} else {
			step("object lock", false, "not enabled on the bucket")
		}
	}

	res.grade(isB2, c.Bucket())
	return res, nil
}

// lockedDeleteRefused tries to destroy the locked test object's version
// and reports whether the provider refused. Any refusal counts: the
// version is still there either way.
func lockedDeleteRefused(ctx context.Context, c *objstore.Client, key string) bool {
	var id string
	c.ListVersions(ctx, key, func(v objstore.Version) error {
		if !v.DeleteMarker && v.Key == key {
			id = v.VersionID
		}
		return nil
	})
	if id == "" {
		return false
	}
	return c.DeleteVersion(ctx, key, id) != nil
}

func bucketOr(b string) string {
	if b == "" {
		return "<bucket>"
	}
	return b
}

func orNone(s string) string {
	if s == "" {
		return "off"
	}
	return s
}

const lockFaultFix = "Object Lock is on, but the locked test upload failed for a reason other than the key's rights — see the object lock step. Re-check once that is resolved; if it persists, it is a fog bug worth reporting."

// grade turns the facts into the letter and the fix.
func (r *ProbeResult) grade(isB2 bool, bucket string) {
	versioned := r.Versioning == "Enabled"
	switch {
	case versioned && r.CanLock && (!r.CanDeleteVersions || r.LockHolds):
		r.Grade = "A"
	case versioned && !r.CanDeleteVersions:
		r.Grade = "B"
		if r.Lock.Enabled && !r.LockDenied {
			r.Fix = lockFaultFix
		} else if isB2 {
			r.Fix = "Enable Object Lock on the bucket (Backblaze console → Buckets → Bucket Settings → Object Lock — it can never be turned off again) and make sure the application key has the writeFileRetentions capability, then re-check."
		} else {
			r.Fix = "Enable Object Lock on the bucket (it can never be turned off again) and allow s3:PutObjectRetention for this key, then re-check."
		}
	case versioned:
		r.Grade = "C"
		switch {
		case r.Lock.Enabled && !r.CanLock && !r.LockDenied:
			r.Fix = lockFaultFix
		case isB2 && r.Lock.Enabled && !r.CanLock:
			r.Fix = "Object Lock is on, but this key cannot lock what it writes. Give fog a key with the writeFileRetentions capability, then re-check — fog locks every object it writes, and no key can destroy a locked version."
		case isB2:
			r.Fix = "Enable Object Lock on the bucket (Backblaze console → Buckets → Bucket Settings → Object Lock — it can never be turned off again), then re-check: fog locks every object it writes, and no key — this one included — can destroy a locked version. " +
				"Backblaze's console cannot make a key without deleteFiles; the b2 command line can: b2 key create --bucket " + bucketOr(bucket) + " fog-offsite " + strings.Join(B2KeyCapabilities, ",") + ". " +
				"Or remove this target and let fog set up a new bucket from your master key."
		case r.Lock.Enabled && !r.CanLock:
			r.Fix = "Object Lock is on, but this key cannot lock what it writes. Allow s3:PutObjectRetention for it, then re-check."
		default:
			r.Fix = "Use a credential whose policy does not allow s3:DeleteObjectVersion (Put, Get, List, and PutObjectRetention are enough), or enable Object Lock on the bucket. Fog never needs to destroy a version."
		}
	default:
		r.Grade = "D"
		if isB2 {
			r.Fix = "Turn on file versioning for the bucket (Bucket Settings → Lifecycle: keep all versions, or keep prior versions for a number of days). Without versions, a delete is a delete."
		} else {
			r.Fix = "Enable bucket versioning so a delete writes a reversible marker instead of destroying the object."
		}
	}
	if versioned {
		if isB2 {
			r.Recommendations = append(r.Recommendations, "Make sure the bucket has a lifecycle rule that deletes hidden files after a week or more (Bucket Settings → Lifecycle → \"Keep prior versions for this number of days\"); fog adds one when it sets the bucket up. Fog only ever hides; the rule is what eventually frees the space.")
		} else {
			r.Recommendations = append(r.Recommendations, "Add a lifecycle rule that expires noncurrent versions after a week or more. Fog only ever writes delete markers; the rule is what eventually frees the space.")
		}
	}
	if r.Grade == "A" && r.Lock.Enabled && r.Lock.Mode != "" && !strings.EqualFold(r.Lock.Mode, "COMPLIANCE") {
		r.Recommendations = append(r.Recommendations, "The bucket's default lock mode is "+r.Lock.Mode+"; fog locks what it writes in compliance mode regardless, which cannot be shortened by anyone.")
	}
}
