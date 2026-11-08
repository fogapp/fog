package main

// root is a type the API encodes, named as the UI knows it. Everything a
// root's fields reference is generated too. Where a handler wraps a domain
// struct in a view (internal/server's *View types), the view is the root:
// it is what actually goes over the wire.
type root struct {
	Path, Go, TS string
}

func (r root) key() typeKey { return typeKey{modulePath + "/" + r.Path, r.Go} }

// output is one generated file (relative to the module root) and what it
// declares.
type output struct {
	File  string
	Roots []root
	Keys  []keySet
}

var outputs = []output{
	{File: "frontend/src/api.gen.ts", Roots: apiRoots, Keys: []keySet{
		// GET /api/settings: every editable setting, plus the read-only
		// flag beside each secret one.
		{Path: "internal/server", Var: "editableSettings", TS: "Settings", Secret: "secretSettings"},
	}},
	{File: "frontend/setup/types.gen.ts", Roots: setupRoots},
	{File: "clients/chat-sdk/src/wire.gen.ts", Roots: chatRoots},
}

// apiRoots are fogd's API, for the control-plane UI.
var apiRoots = []root{
	// Apps
	{"internal/apps", "AppSpec", ""},
	{"internal/apps", "AppStatus", ""},
	{"internal/apps", "GitInfo", ""},
	{"internal/apps", "Diagnosis", ""},
	{"internal/apps", "ContainerFacts", "AppContainer"},
	{"internal/apps", "Review", "AppReview"},
	{"internal/server", "importResult", "AppImportResult"},
	{"internal/store", "App", "AppRecord"},
	{"internal/store", "AppRevision", ""},
	{"internal/store", "Deploy", "DeployRecord"},
	{"internal/store", "FederationFacts", ""},
	{"internal/server", "appVersionRow", "AppVersion"},
	{"internal/server", "appVersionsResponse", "AppVersionsResponse"},
	{"internal/build", "RepoDoc", ""},
	{"internal/backup", "AppBackupStatus", ""},
	{"internal/backup", "StoreSummary", "BackupStoreSummary"},
	{"internal/store", "Backup", "BackupRecord"},
	{"internal/updates", "AppUpdateStatus", ""},
	{"internal/updates", "CheckResult", "UpdateCheckResult"},
	{"internal/integrations", "List", "IntegrationsList"},
	{"internal/integrations", "Entry", "IntegrationEntry"},
	{"internal/catalog", "Entry", "CatalogEntry"},
	{"internal/catalog", "Host", "CapacityHost"},
	{"internal/catalog", "Installed", "CapacityApp"},
	{"internal/catalog", "Fit", ""},
	{"internal/server", "catalogResponse", "CatalogResponse"},
	{"internal/catalog", "Status", "CatalogStatus"},
	{"internal/selfupdate", "CheckResult", "SelfUpdateStatus"},
	{"internal/dockerx", "InstallStatus", "DockerInstallStatus"},
	{"internal/server", "containerFacts", "ContainerFactsT"},

	// Compute nodes
	{"internal/server", "computeOverview", "ComputeOverview"},
	{"internal/compute", "Machine", "ComputeMachine"},
	{"internal/compute", "Pool", "ComputePool"},
	{"internal/compute", "Usage", "ComputeUsage"},
	{"internal/store", "ComputeActivity", "ComputeActivity"},

	// Health and housekeeping
	{"internal/server", "issueView", "Issue"},
	{"internal/store", "ErrorGroup", ""},
	{"internal/server", "errorGroupWithApp", "FleetErrorGroup"},
	{"internal/store", "Notice", ""},
	{"internal/store", "AuditEntry", ""},
	{"internal/janitor", "Orphan", ""},
	{"internal/metrics", "Sample", "MetricSample"},
	{"internal/dockerx", "Version", "DockerVersion"},
	{"internal/dockerx", "Info", "DockerInfo"},
	{"internal/server", "diskEntry", "Disk"},
	{"internal/server", "diskSegment", "DiskSegment"},
	{"internal/server", "fsEntry", "FsEntry"},
	{"internal/server", "fsListResponse", "FsListing"},

	// People and access
	{"internal/server", "personAdminView", "PersonAdmin"},
	{"internal/store", "PersonInvite", ""},
	{"internal/store", "MCPToken", ""},

	// Files
	{"internal/files", "Node", "FileNode"},
	{"internal/files", "Listing", "FilesListing"},
	{"internal/files", "Detail", "FileDetail"},
	{"internal/files", "Status", "FilesStatus"},
	{"internal/files", "TrashItem", ""},
	{"internal/server", "filePinView", "FilePin"},
	{"internal/store", "FileShare", ""},
	{"internal/store", "FileSnapshot", ""},
	{"internal/files", "BackupStatus", "FilesBackupStatus"},
	{"internal/filesfed", "CatalogEntry", "FileFedCatalogEntry"},
	{"internal/store", "FilesAccessToken", ""},
	{"internal/filesfed", "ShareView", "FileFedShare"},
	{"internal/store", "FileFedSubscription", ""},
	{"internal/nas", "ShareStatus", "NasShare"},

	// Offsite
	{"internal/server", "offsiteTargetView", "OffsiteTarget"},
	{"internal/offsite", "B2SetupReport", ""},
	{"internal/offsite", "Preset", "OffsitePreset"},
	{"internal/offsite", "LockerPreview", ""},
	{"internal/offsite", "RecoverResult", ""},

	// Federation
	{"internal/server", "fedPeerView", "FedPeer"},
	{"internal/fed", "DirectoryApp", "FedDirectoryApp"},
	{"internal/store", "FedInvite", ""},
	{"internal/server", "fedIdentityView", "FedIdentity"},
	{"internal/server", "fedReachability", "FedReachability"},
	{"internal/server", "ledgerEntry", "FederationLedgerEntry"},

	// Reach
	{"internal/tunnel", "Status", "TunnelStatus"},
	{"internal/vpn", "Status", "VpnStatus"},
	{"internal/sshaccess", "Status", "SshAccessStatus"},

	// Feedback
	{"internal/server", "feedbackView", "FeedbackItem"},
	{"internal/server", "feedbackAttached", "FeedbackAttached"},
	{"internal/feedback", "Dossier", "FeedbackDossier"},

	// Envelopes handlers used to build from maps
	{"internal/server", "systemStatus", "SystemStatus"},
	{"internal/server", "meView", "Me"},
	{"internal/server", "personMe", "PersonMe"},
	{"internal/server", "setupStatus", "SetupStatus"},
	{"internal/server", "federationInfo", "FederationInfo"},
	{"internal/server", "offsiteTargetsResponse", "OffsiteTargetsResponse"},
	{"internal/server", "deployStatusResponse", "DeployStatusResponse"},
	{"internal/server", "liveDeploysResponse", "LiveDeploysResponse"},
	{"internal/server", "inboxSummary", "InboxSummary"},
	{"internal/server", "offsiteSaved", "OffsiteSaved"},
	{"internal/server", "offsiteRowsResponse", "OffsiteRowsResponse"},
	{"internal/server", "offsiteFilesResponse", "OffsiteFilesResponse"},
	{"internal/server", "offsitePresetsResponse", "OffsitePresetsResponse"},
	{"internal/server", "offsiteJobStarted", "OffsiteJobStarted"},
	{"internal/server", "peopleDirectory", "PeopleDirectory"},
	{"internal/server", "personInviteCreated", "PersonInviteCreated"},
	{"internal/server", "joinPreview", "JoinPreview"},
	{"internal/server", "personSignedIn", "PersonSignedIn"},
	{"internal/server", "passkeyCeremony", "PasskeyCeremony"},
}

// setupRoots are the fog-setup agent's API (internal/setup), for the
// installer wizard.
var setupRoots = []root{
	{"internal/setup", "Inventory", ""},
	{"internal/setup", "AgentState", ""},
}

// chatRoots are the chat and chat-people responses the SDK reads. Responses
// a handler builds from a map stay hand-written in the SDK's api.ts.
var chatRoots = []root{
	{"internal/server", "roomView", "RoomView"},
	{"internal/server", "participantView", "ParticipantView"},
	{"internal/server", "eventView", "EventView"},
	{"internal/server", "deviceView", "DeviceView"},
	{"internal/server", "directoryPerson", "DirectoryPerson"},
	{"internal/server", "keyMaterial", "KeyMaterial"},
	{"internal/store", "PersonSessionView", "SessionView"},
	{"internal/store", "ChatWebhook", "Webhook"},
	{"internal/chat", "Prefs", ""},
	{"internal/chat", "Preview", "PreviewCardResponse"},
}

// renames names referenced types whose Go name is unexported, would collide,
// or would shadow a DOM global (File), keyed by import path and type.
var renames = map[string]string{
	modulePath + "/internal/apps.Source":                 "AppSource",
	modulePath + "/internal/apps.File":                   "FileMount",
	modulePath + "/internal/apps.Route":                  "RouteSpec",
	modulePath + "/internal/apps.Port":                   "PortSpec",
	modulePath + "/internal/apps.Health":                 "HealthSpec",
	modulePath + "/internal/apps.Needs":                  "AppNeeds",
	modulePath + "/internal/apps.Process":                "ProcessSpec",
	modulePath + "/internal/files.Mount":                 "FolderMount",
	modulePath + "/internal/files.PeerMark":              "FilePeerMark",
	modulePath + "/internal/files.Sort":                  "FilesSort",
	modulePath + "/internal/files.Stats":                 "FilesStats",
	modulePath + "/internal/offsite.ProbeResult":         "OffsiteProbe",
	modulePath + "/internal/offsite.ProbeStep":           "OffsiteProbeStep",
	modulePath + "/internal/offsite.IndexKit":            "LockerKit",
	modulePath + "/internal/offsite.IndexFiles":          "LockerFiles",
	modulePath + "/internal/objstore.LockConfig":         "BucketLock",
	modulePath + "/internal/store.OffsiteCopyTotals":     "OffsiteTotals",
	modulePath + "/internal/jobs.Job":                    "Job",
	modulePath + "/internal/catalog.Option":              "CatalogOption",
	modulePath + "/internal/catalog.Companion":           "CatalogCompanion",
	modulePath + "/internal/catalog.Listing":             "CatalogListing",
	modulePath + "/internal/catalog.Screenshot":          "CatalogScreenshot",
	modulePath + "/internal/catalog.Requires":            "CatalogRequires",
	modulePath + "/internal/catalog.Problem":             "CatalogProblem",
	modulePath + "/internal/vpn.Snapshot":                "VpnSnapshot",
	modulePath + "/internal/sshaccess.Key":               "SshKey",
	modulePath + "/internal/server.diskUses":             "DiskUses",
	modulePath + "/internal/server.feedbackDeliveryView": "FeedbackDelivery",
	modulePath + "/internal/server.tunnelReach":          "TunnelReach",
	modulePath + "/internal/feedback.Issue":              "DossierIssue",
	modulePath + "/internal/feedback.Notice":             "DossierNotice",
	modulePath + "/internal/feedback.Page":               "DossierPage",
	modulePath + "/internal/feedback.Fog":                "DossierFog",
	modulePath + "/internal/feedback.App":                "DossierApp",
	modulePath + "/internal/feedback.Container":          "DossierContainer",
	modulePath + "/internal/feedback.Error":              "DossierError",
	modulePath + "/internal/feedback.Peer":               "DossierPeer",
	modulePath + "/internal/logring.Record":              "LogRecord",
	modulePath + "/internal/server.directoryDv":          "DirectoryDevice",
	modulePath + "/internal/server.keyMaterialDevice":    "KeyMaterialDevice",
	modulePath + "/internal/chat.Role":                   "PolicyRole",
	modulePath + "/internal/chat.Retention":              "PolicyRetention",
}
