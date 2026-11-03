package offsite

import "strings"

// Preset is a provider's shape: what the add-target form pre-fills and
// what it tells the operator to set up on the provider's side. The wire
// is S3 for every one of them.
type Preset struct {
	ID       string `json:"id"`
	Label    string `json:"label"`
	Endpoint string `json:"endpoint"` // example, with <placeholders>
	Region   string `json:"region"`   // example or fixed value
	// PathStyle is the addressing the provider prefers.
	PathStyle bool `json:"pathStyle"`
	// PricePerTB is the list storage price, USD per TB-month, for the cost
	// meter's default.
	PricePerTB float64 `json:"pricePerTb"`
	// Setup is what to do on the provider's side, in order, to reach
	// grade A. Step text is shown verbatim on the form.
	Setup []string `json:"setup"`
	// Warning is said before the operator creates anything: the one-way
	// door.
	Warning string `json:"warning"`
}

const lockWarning = "Object Lock is a one-way door: once enabled on a bucket it can never be turned off, and every locked object stays — and is billed — until its retention lapses, no matter who asks, including you. That is exactly what a backup wants and exactly what a \"let me tidy this up\" wants least. Fog locks what it writes for the span of the recent tier plus a week, and re-locks what the monthly and yearly tiers keep."

// Presets is the list the form offers, in display order.
var Presets = []Preset{
	{
		ID: "b2", Label: "Backblaze B2",
		Endpoint: "https://s3.<region>.backblazeb2.com", Region: "us-west-004", PathStyle: true, PricePerTB: 6.95,
		Setup: []string{
			"Create a bucket (private). Turn on Object Lock in the bucket's settings — recommended, and permanent.",
			"Add a lifecycle rule: keep prior versions for 14 days (or more). Fog only ever hides; this rule is what frees the space.",
			"Create an application key restricted to that bucket. Backblaze's console only makes keys that can delete (fine with Object Lock on: nothing locked can be deleted). For a key that cannot, use the b2 command line: b2 key create --bucket <bucket> fog-offsite " + strings.Join(B2KeyCapabilities, ",") + " — or go back and let fog set the bucket up from your master key.",
			"The endpoint is shown on the bucket's page as its S3 endpoint; the region is the part between s3. and .backblazeb2.com.",
		},
		Warning: lockWarning,
	},
	{
		ID: "r2", Label: "Cloudflare R2",
		Endpoint: "https://<account-id>.r2.cloudflarestorage.com", Region: "auto", PathStyle: true, PricePerTB: 15,
		Setup: []string{
			"Create a bucket. R2 has no Object Lock and no bucket versioning: every target here grades D — a delete is a delete. Fog still works; the history is only as safe as the credential.",
			"Create an R2 API token scoped to the bucket with Object Read & Write.",
		},
		Warning: "",
	},
	{
		ID: "wasabi", Label: "Wasabi",
		Endpoint: "https://s3.<region>.wasabisys.com", Region: "us-east-1", PathStyle: true, PricePerTB: 6.99,
		Setup: []string{
			"Create a bucket with versioning and Object Lock enabled (both at creation; lock is permanent).",
			"Create a user and access key whose policy allows s3:PutObject, GetObject, ListBucket, ListBucketVersions, GetBucketVersioning, GetBucketObjectLockConfiguration, PutObjectRetention, and DeleteObject — but not s3:DeleteObjectVersion.",
			"Wasabi bills a 90-day minimum per object; the yearly tier is where that is cheapest.",
		},
		Warning: lockWarning,
	},
	{
		ID: "s3", Label: "Amazon S3 or another S3-compatible service",
		Endpoint: "https://s3.<region>.amazonaws.com", Region: "us-east-1", PathStyle: false, PricePerTB: 23,
		Setup: []string{
			"Create a bucket with versioning on; enable Object Lock for immutability (permanent).",
			"Give the credential Put, Get, List, GetBucketVersioning, GetBucketObjectLockConfiguration and PutObjectRetention on the bucket — and not DeleteObjectVersion.",
			"Add a lifecycle rule expiring noncurrent versions after 14 days or more.",
			"MinIO and Garage work the same way; MinIO needs versioning enabled per bucket, and Object Lock at bucket creation.",
		},
		Warning: lockWarning,
	},
}

// PresetByID returns the preset or nil.
func PresetByID(id string) *Preset {
	for i := range Presets {
		if Presets[i].ID == id {
			return &Presets[i]
		}
	}
	return nil
}

// IsB2 reports whether an endpoint is Backblaze's — the fix text speaks
// B2's vocabulary (application keys, capabilities, hide) when it is.
func IsB2(endpoint string) bool {
	return strings.Contains(strings.ToLower(endpoint), "backblazeb2.com")
}
