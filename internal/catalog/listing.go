package catalog

// The store front. An Entry's spec says how an app runs; its listing says
// what it is, in the terms a person choosing between two apps thinks in —
// the paragraph, the screenshots, who makes it and under what licence. The
// listing is display copy only: nothing here reaches a container.
//
// Artwork lives under frontend/public/store/<id>/ and ships inside the
// binary with the rest of the UI, so a store page never fetches from the
// internet — a Fog box on a LAN with no route out still gets its logos.
// Logos are the upstream projects' own marks (via the dashboard-icons
// collection); screenshots are the ones the projects publish in their own
// READMEs and sites. The first-party apps carry tiles drawn for Fog.

// Screenshot is one picture on a listing, with the words a screen reader
// says instead — which is also the caption under it.
type Screenshot struct {
	Src string `json:"src"`
	Alt string `json:"alt"`
}

// Listing is the store-front half of an Entry, kept apart from the spec so
// the catalog file stays about how apps run.
type Listing struct {
	// About is the pitch, two or three short markdown paragraphs. It says
	// what the app is for and what it is like to live with — not a feature
	// list; that is Highlights.
	About string `json:"about,omitempty"`
	// Highlights are three to six short feature lines, each a sentence
	// fragment beginning with the capability.
	Highlights []string `json:"highlights,omitempty"`
	// Screenshots in display order; the first is the one a card leads with.
	Screenshots []Screenshot `json:"screenshots,omitempty"`
	// Logo is the path of the app's mark under the UI's static files. A
	// square image; the UI draws the rounded tile around it.
	Logo      string `json:"logo,omitempty"`
	Developer string `json:"developer,omitempty"`
	License   string `json:"license,omitempty"`
	// Website is the project's front door; Docs its manual. Either may be
	// empty; ProjectURL on the entry is the source repository.
	Website string `json:"website,omitempty"`
	Docs    string `json:"docs,omitempty"`
}

// logo is a project's mark on a transparent ground; the UI sets it inside
// its own tile with a margin. tile is an icon drawn edge to edge — the
// tile is part of the picture, and the UI shows it as is. The file name
// carries the distinction so the UI needs no second field to read.
func logo(id string) string { return "/store/" + id + "/logo.svg" }

func logoWebp(id string) string { return "/store/" + id + "/logo.webp" }

func tile(id string) string { return "/store/" + id + "/tile.svg" }

func shots(id string, alts ...string) []Screenshot {
	out := make([]Screenshot, 0, len(alts))
	for i, alt := range alts {
		out = append(out, Screenshot{Src: "/store/" + id + "/shot-" + string(rune('1'+i)) + ".webp", Alt: alt})
	}
	return out
}

// firstParty is the developer line the household's own apps share: they
// are built from a repository the operator owns, not fetched from a
// registry.
const firstParty = "Built from your own repository"

var listings = map[string]Listing{
	"fin": {
		About: "Fin is a household's financial life in one place. It pulls bank, card, brokerage, crypto, property, and equity-compensation data into a single normalised ledger, keeps it current on its own, and gives you the views a good personal finance app would — cashflow, budget, subscriptions, net worth, portfolio performance, taxes.\n\n" +
			"Every number traces back to the transactions that produced it, and everything stays on your hardware. It is built for one household — a few people, one or more admins — so it favours clarity and correctness over multi-tenant scale.",
		Highlights: []string{
			"Plaid, CSV imports, Coinbase and on-chain wallets land in one ledger",
			"Cashflow, budgets, subscriptions, and net worth over time",
			"Portfolio performance with cost basis and daily prices",
			"Workflows: event-driven rules that categorise, tag, notify, and sync",
			"Recurring charges and transfers detected from cadence, not tagging",
			"Passkey sign-in, MCP access for agents, webhooks",
		},
		Logo: tile("fin"), Developer: firstParty, License: "Private",
	},
	"hub": {
		About: "Hub is a private home base for a household: the people, plans, and paperwork that keep the place running. One household per instance, one login per person, and everything else — photos, calendars, mail, documents — pulled in from the self-hosted services you already run.\n\n" +
			"The dashboard is a shared chronicle: a journal with mentions, photos, comments, and reactions, alongside who lives here, what is coming up, and the photo days Hub thinks are worth writing up.",
		Highlights: []string{
			"People and pets: contacts, places, health, education, and a derived family tree",
			"Calendars of its own plus CalDAV, ICS subscriptions, and a wall-tablet kiosk view",
			"Vaults for property, vehicles, insurance, accounts, and documents, with expiry reminders",
			"Todos as kanban boards with checklists and links to any record",
			"Pulls photos from Immich and documents from Paperless into the chronicle",
			"Transcribes podcasts through Whisper",
		},
		Logo: tile("hub"), Developer: firstParty, License: "Private",
	},
	"office": {
		About: "Office is a document editor for a household: a private place to write, keep, and share the letters, reports, and notes a family accumulates, with a real word processor in the browser rather than a text box.\n\n" +
			"Documents open from and save back to the household's own files — .docx, Markdown, plain text, HTML, PDF — so nothing is locked in, and every edit is autosaved and versioned so nothing is lost.",
		Highlights: []string{
			"A document window: menu bar, toolbar, ruler, paper, and a command palette",
			"Headings, lists, tables, images, footnotes, a table of contents, page setup",
			"Several people in one document at once, with presence and remote cursors",
			"Revision history with named versions, rendered diffs, and one-click restore",
			"Comments anchored to a passage, with mentions, replies, and resolve",
			"Exact PDF export and legacy formats through Gotenberg",
		},
		Logo: tile("office"), Developer: firstParty, License: "Private",
	},
	"feed": {
		About: "Feed is a curation engine. It pulls everything from the people you explicitly follow — Reddit, YouTube, Bluesky, Mastodon, Twitch, podcasts, blogs, newsletters — into a permanent local archive, runs your algorithm over it, and serves the result out through whatever you like to read in.\n\n" +
			"No discovery, no suggestions, no feed you didn't ask for. Feeds that end, an algorithm you can diff like code, and a per-item answer to “why am I seeing this”.",
		Highlights: []string{
			"Follow-only, full-fidelity, archival-grade ingestion",
			"Rules, scoring, topics, transcripts, and dedup you can read and change",
			"Google Reader-compatible sync: NetNewsWire, Reeder, and friends just work",
			"RSS, Atom, podcast feeds, ActivityPub actors, and MCP for agents",
			"Peer Feed instances are first-class sources",
			"Alerts through ntfy",
		},
		Logo: tile("feed"), Developer: firstParty, License: "Private",
	},
	"hook": {
		About: "Hook is an acquisition engine for ebooks and audiobooks. You tell it what you want — a book, an author, a series — and it obtains the right edition from the usenet indexers you subscribe to, through the download client you already run, and hands the finished file to BookOrbit, where you actually read.\n\n" +
			"It fills the role Readarr played in a media stack, built so that a single closed metadata proxy can never kill it, and so that every step between “I want this” and “it is on my shelf” is legible and self-correcting.",
		Highlights: []string{
			"Wants, profiles, and monitors for authors and series",
			"A scored decision engine that explains every grab",
			"A failure ladder that retries, substitutes, and asks for help in that order",
			"Indexers from Prowlarr, downloads through SABnzbd, delivery to BookOrbit",
			"Credentials stored encrypted in Settings, never in environment variables",
			"Grab and review alerts through ntfy",
		},
		Logo: tile("hook"), Developer: firstParty, License: "Private",
	},

	"once": {
		About: "Once makes One Second Everyday films from the household's Immich library. Pick the people — or a group of them — and a stretch of dates, and Once lays out every day they appear on a calendar: one second from each video, a slow Ken Burns move across each photo, and a Live Photo's motion where there is one.\n\n" +
			"Drag a second to another day, choose which second of a clip to keep, preview the whole film, then add music and export one MP4 to the household's video library. Immich keeps the originals; Once only keeps the choices you made.",
		Highlights: []string{
			"People and groups straight from Immich's faces",
			"A calendar of days, rearranged by dragging",
			"The exact second of each video, and the move across each photo",
			"Music from an upload, a YouTube link, or the household's library, ducked under the originals",
			"Landscape or portrait films, saved to Videos or back into an Immich album",
		},
		Logo: tile("once"), Developer: firstParty, License: "Private",
	},

	"stream": {
		About: "Stream is the household's video player. It reads the video already on the network — movies, shows, and everything that is neither: YouTube archives, home videos, lectures, concerts — and plays it in the browser instantly, at original quality whenever the browser can take it.\n\n" +
			"It takes Plex's polish, Jellyfin's openness, and YouTube's grammar for loose video, and adds the thing none of them have: a player that tells you why it did what it did. It only ever reads your media folders; everything it learns lives in its own database.",
		Highlights: []string{
			"Movies, shows, and loose video each browse the way they should",
			"Direct play first — transcoding is a last resort, and explained",
			"Files identified by content, so renames never lose watch history",
			"Continue watching and resume everywhere, per profile",
			"Metadata and artwork from TMDB, new arrivals from Sonarr and Radarr webhooks",
			"Watch history and positions migrated from Plex",
		},
		Logo: tile("stream"), Developer: firstParty, License: "Private",
	},

	"hud": {
		About: "HUD brings timely information to the edge of attention without interrupting work. It connects sources — calendars, webhooks, agents — to surfaces — a BusyBar on the desk, a phone, the room's lights — through one pipeline of rules, so the next meeting counts down quietly instead of popping up over what you are doing.\n\n" +
			"Every notice can be traced back to the event and the rule that raised it, and rules are edited and simulated live in the console.",
		Highlights: []string{
			"Google Calendar, CalDAV, and ICS feeds",
			"CloudEvents webhooks and MCP for agents",
			"BUSY Bar: native countdown, details, LED, chime, and buttons",
			"Phone notices through ntfy, room lights through Home Assistant",
			"Rules in CEL with a live editor and simulator",
			"A trace of why every notice appeared",
		},
		Logo: tile("hud"), Developer: firstParty, License: "Private",
	},

	"lucid": {
		About: "Lucid is how you see through the fog: one console for everything the apps on this server are doing — metrics, request traces, logs, continuous profiles, and visitor analytics, all behind one query bar.\n\n" +
			"An app opts in with an address, a key, and one Gemfile line, and reports from its next boot — no collector, no YAML, nothing to add to its frontend. Telemetry is kept within a disk budget you set and is left out of backups; the configuration is not.",
		Highlights: []string{
			"Requests, SQL, views, cache, jobs, and outbound HTTP, traced end to end",
			"Logs and runtime vitals beside the traces they belong to",
			"Continuous profiling with flamegraphs",
			"Pageviews, clicks, and web vitals, relayed by the app itself",
			"One query bar across every signal",
			"Alerts to ntfy, and a disk budget for telemetry",
		},
		Logo: tile("lucid"), Developer: firstParty, License: "Private",
	},

	"newtab": {
		About: "Newtab is the household's start page: every app on this server one keystroke away, with live content from them — what's playing on Stream, what Feed ranked this morning, what fog's Inbox is waiting on — in a page that paints before the browser has finished opening the tab.\n\n" +
			"It knows every app fog runs without being told. A repeat visit paints from the device with no requests, and stale content is shown, marked, and replaced in place rather than waited for.",
		Highlights: []string{
			"Every fog app, with no configuration",
			"Live widgets from the household's apps",
			"Paints from cache with zero network on a repeat visit",
			"An in-page editor on a 12-column grid, saved to newtab.yaml",
			"A browser extension that makes it the new-tab page",
		},
		Logo: tile("newtab"), Developer: firstParty, License: "Private",
	},

	"fit": {
		About: "Fit is a household's health record on its own hardware. It pulls in Whoop, Withings and Apple Health, tracks medication and supplement regimens, and keeps every number traceable to the payload that produced it.\n\n" +
			"Health data stays in the house. Fit runs on a sealed network with no route out of the machine, answers on the home network only, and offers nothing to other apps. Morning briefs and lab reading use the household's own models or wait.",
		Highlights: []string{
			"Whoop and Withings through a separate collector that can't read Fit",
			"Apple Health and one-tap dose logging from the iPhone app",
			"Every number traces back to its raw payload, kept forever",
			"Row-level security per person: admin grants no access to anyone's health data",
			"Briefs and lab reading on household models, with no cloud fallback",
		},
		Logo: tile("fit"), Developer: firstParty, License: "Private",
	},
	"think-fleet": {
		About: "think fleet is where the household's Macs running think meet. Each Mac keeps its own record of the models it has tried, run, benchmarked and judged; the hub holds all of them, so you can compare the same model across machines and see what another Mac made of a model before you download it.\n\n" +
			"Model files are kept once for the house, on a NAS if you like, and copied to a Mac over the LAN instead of downloaded again. Overnight, idle Macs measure what the others haven't, and one endpoint answers for the whole house from whichever Mac has the model warm. Prompts people send never reach the hub.",
		Highlights: []string{
			"One record of every model each Mac has tried, run and scored",
			"The same model compared across machines, with every setting that differs called out",
			"Model files downloaded once for the house and copied over the LAN, checked by hash",
			"Overnight benchmarks on idle Macs, and speed predictions for models nobody has downloaded",
			"One OpenAI-compatible endpoint for the house, routed to the Mac best placed to answer",
		},
		Logo: tile("think-fleet"), Developer: firstParty, License: "Private",
	},
	"fit-collector": {
		About: "Fit Collector is the one part of Fit with internet access. It holds the Whoop and Withings tokens and fetches on a schedule, so that Fit itself never needs a route out of the machine.\n\n" +
			"It has no database and none of Fit's secrets. Fit pulls from it over a token only Fit can ask for, and nothing flows the other way.",
		Highlights: []string{
			"Holds the vendor tokens, encrypted, apart from Fit",
			"No database and no way to read Fit",
			"Offers its data to Fit alone",
		},
		Logo: tile("fit-collector"), Developer: firstParty, License: "Private",
	},

	"signet": {
		About: "Signet is personal capture-and-recall: save a page, a thought, a screenshot or a video from any device in one gesture, along with why and where you saved it — and get it back exactly when it matters.\n\n" +
			"Search blends keywords with meaning over everything you have saved, and an MCP server brings your library into AI agent sessions. Every item is also mirrored to plain files, readable without Signet.",
		Highlights: []string{
			"Save from the browser, Mac, iPhone, email or Telegram",
			"Remembers why you saved something, not just what",
			"Hybrid keyword and semantic search",
			"Recall inside Claude Code and other MCP clients",
			"A plain-file vault that outlives the database",
		},
		Logo: tile("signet"), Developer: firstParty, License: "Private",
	},

	"immich": {
		About: "Immich is a photo and video library for a household — the self-hosted answer to Google Photos and iCloud. Phones back up to it automatically, and everything is searchable by person, place, date, or a plain description of what is in the picture.\n\n" +
			"It runs with its own database, cache, and a machine-learning service that recognises faces and objects on this machine, so nothing leaves the house to be indexed.",
		Highlights: []string{
			"Automatic backup from the iOS and Android apps",
			"Face recognition, smart search, and a map of where photos were taken",
			"Shared albums, partner sharing, and public links",
			"Timeline, memories, and duplicate detection",
			"RAW, HEIC, live photos, and 360° images handled natively",
			"Multi-user with per-user libraries and storage quotas",
		},
		Screenshots: shots("immich", "Immich on the web, iPhone, and Android: the timeline, a map, and a photo detail"),
		Logo:        logo("immich"), Developer: "Immich (FUTO)", License: "AGPL-3.0",
		Website: "https://immich.app", Docs: "https://immich.app/docs",
	},
	"paperless": {
		About: "Paperless-ngx is a document archive. Scan or drop a document in, and it is OCR'd, tagged, filed, and made searchable — the paper in the house becomes something you can find again from any device.\n\n" +
			"It learns from how you file: correspondents, document types, and tags are suggested from the text, and mail rules can pull attachments in from a mailbox on their own.",
		Highlights: []string{
			"OCR in your language, with full-text search across everything",
			"Automatic tagging and filing that learns from your corrections",
			"A scans folder: drop a file in and it is consumed",
			"Mail rules that fetch attachments from an inbox",
			"Custom fields, workflows, and a share link for any document",
			"Per-user permissions for shared households",
		},
		Screenshots: shots("paperless", "The dashboard: saved views, statistics, and recent documents", "The documents view in dark mode, as small cards", "Editing a document's metadata beside its preview", "The documents view as a table"),
		Logo:        logo("paperless"), Developer: "Paperless-ngx community", License: "GPL-3.0",
		Website: "https://docs.paperless-ngx.com", Docs: "https://docs.paperless-ngx.com",
	},
	"whisper": {
		About: "A speech-to-text server built on OpenAI's Whisper model, served as an HTTP API. It transcribes and translates audio on this machine, so nothing recorded here leaves it.\n\n" +
			"There is no interface to speak of — it is a service other apps call. Hub uses it to transcribe podcasts; anything that can POST an audio file can use it too.",
		Highlights: []string{
			"Transcription and translation from any common audio format",
			"Model size is your choice: tiny for speed, large for accuracy",
			"Word-level timestamps and SRT, VTT, or JSON output",
			"Language detection",
			"A plain HTTP API with interactive docs at /docs",
		},
		Logo: tile("whisper"), Developer: "Ahmet Öner", License: "MIT",
		Docs: "https://ahmetoner.com/whisper-asr-webservice/",
	},
	"openclaw": {
		About: "OpenClaw is a personal AI assistant that lives on your own hardware and meets you where you already talk — WhatsApp, Telegram, Discord, Slack, iMessage, and the web — with memory that persists between conversations and tools it can act with.\n\n" +
			"State, memory, and credentials stay on this machine. Models are plugins: point it at Claude, a local model, or whatever you prefer, and swap later without changing anything else.",
		Highlights: []string{
			"One assistant across twenty-plus chat channels and native apps",
			"Long-term memory and per-person context",
			"Tools and skills: calendars, home automation, browsing, code",
			"Any model provider, including ones running locally",
			"No paid tier, hosted service, or token — stewarded by a foundation",
		},
		Screenshots: shots("openclaw", "OpenClaw's mark", "The agents view of the web interface"),
		Logo:        logo("openclaw"), Developer: "OpenClaw Foundation", License: "MIT",
		Website: "https://openclaw.ai", Docs: "https://docs.openclaw.ai",
	},
	"bookorbit": {
		About: "BookOrbit is a library and reading platform for ebooks, PDFs, audiobooks, and comics. It organises what you own and lets you read it anywhere — the web, an iPhone, an Apple Watch, a Kobo, or KOReader — with progress, highlights, and status kept in sync between them.\n\n" +
			"Around that core sit metadata from a dozen providers, reading statistics, OPDS and send-to-Kindle delivery, and multi-user accounts, all on your own hardware.",
		Highlights: []string{
			"Built-in readers for EPUB, PDF, comics, and audiobooks — no plugins",
			"Three-way sync between Kobo, KOReader, and the web reader",
			"Native iPhone and Apple Watch apps with offline downloads",
			"Metadata from fourteen providers, reading stats, and achievements",
			"OPDS, send-to-Kindle, and sync out to Hardcover, Readwise, and StoryGraph",
			"Where Hook delivers the books it finds",
		},
		Screenshots: shots("bookorbit", "The dashboard: reading stats, widgets, and shelves", "BookOrbit on iPhone and Apple Watch", "The KOReader plugin on an e-reader"),
		Logo:        logoWebp("bookorbit"), Developer: "neon and BookOrbit contributors", License: "AGPL-3.0",
		Website: "https://bookorbit.app",
	},

	"sonarr": {
		About: "Sonarr keeps a TV library current. Tell it the series you follow, and it watches your indexers for new episodes, sends the best match to your download client, and renames and files the result where Jellyfin expects it.\n\n" +
			"It is the heart of a media stack: Prowlarr feeds it indexers, qBittorrent or SABnzbd does the downloading, and Bazarr adds subtitles afterwards.",
		Highlights: []string{
			"Monitors series for new and missing episodes",
			"Quality profiles that upgrade a copy when a better one appears",
			"Automatic renaming and organisation on import",
			"A calendar of what airs when",
			"Usenet and torrent clients alike",
		},
		Logo: logo("sonarr"), Developer: "Sonarr team", License: "GPL-3.0",
		Website: "https://sonarr.tv", Docs: "https://wiki.servarr.com/sonarr",
	},
	"radarr": {
		About: "Radarr does for films what Sonarr does for series. Add a film — or a list of them — and it finds a copy that meets your quality profile, downloads it through your client, and files it under a clean name for Jellyfin.\n\n" +
			"Lists from TMDB, IMDb, Trakt, and Letterboxd keep the wanted queue full without you adding titles one by one.",
		Highlights: []string{
			"Discover and add from lists, or search by title",
			"Quality profiles with automatic upgrades",
			"Renaming and folder structure on import",
			"Usenet and torrent clients alike",
			"Custom formats for the releases you prefer",
		},
		Screenshots: shots("radarr", "Discovering films to add", "A film's page with its releases"),
		Logo:        logo("radarr"), Developer: "Radarr team", License: "GPL-3.0",
		Website: "https://radarr.video", Docs: "https://wiki.servarr.com/radarr",
	},
	"lidarr": {
		About: "Lidarr manages a music collection the way Sonarr manages series. Follow artists and it watches for new releases, grabs them through your download client, and files them tagged and named for Navidrome.\n\n" +
			"It handles the whole discography: albums, EPs, singles, and the metadata that keeps a library tidy.",
		Highlights: []string{
			"Follow artists and monitor for new releases",
			"Quality and metadata profiles",
			"Tags and renames on import",
			"Usenet and torrent clients alike",
		},
		Screenshots: shots("lidarr", "An artist's page in Lidarr", "Adding a new artist"),
		Logo:        logo("lidarr"), Developer: "Lidarr team", License: "GPL-3.0",
		Website: "https://lidarr.audio", Docs: "https://wiki.servarr.com/lidarr",
	},
	"prowlarr": {
		About: "Prowlarr is the one place your indexers are configured. Add a usenet or torrent indexer once and it is shared with Sonarr, Radarr, Lidarr, and Hook automatically — no re-entering API keys in every app.\n\n" +
			"It also searches across every indexer at once, and keeps the history of what was asked and what came back.",
		Highlights: []string{
			"Hundreds of indexer definitions built in",
			"Syncs indexers to every arr app you connect",
			"Search all indexers from one screen",
			"Proxy and rate-limit support",
		},
		Screenshots: shots("prowlarr", "Searching every indexer at once"),
		Logo:        logo("prowlarr"), Developer: "Prowlarr team", License: "GPL-3.0",
		Website: "https://prowlarr.com", Docs: "https://wiki.servarr.com/prowlarr",
	},
	"bazarr": {
		About: "Bazarr finds subtitles for the films and series Sonarr and Radarr manage. It watches the library, fetches subtitles in the languages you want from a dozen providers, and keeps upgrading them as better ones appear.\n\n" +
			"It fills the gap Jellyfin can't: subtitles that arrive on their own, in the right language, for everything.",
		Highlights: []string{
			"Language profiles per series or film",
			"A dozen subtitle providers",
			"Automatic upgrades to better-synced subtitles",
			"Follows Sonarr's and Radarr's libraries directly",
		},
		Screenshots: shots("bazarr", "Bazarr's series view with subtitle status"),
		Logo:        logo("bazarr"), Developer: "morpheus65535", License: "GPL-3.0",
		Website: "https://www.bazarr.media", Docs: "https://wiki.bazarr.media",
	},
	"jellyseerr": {
		About: "Jellyseerr is the front door to a media stack for people who don't want to see the machinery. Anyone in the household signs in with their Jellyfin account, searches for a film or series, and presses Request; Sonarr and Radarr do the rest.\n\n" +
			"Requests can need approval, and everyone can see what is already in the library, what is on its way, and what is coming out soon.",
		Highlights: []string{
			"Sign in with a Jellyfin account",
			"Request films and series; approve or auto-approve per user",
			"Shows what is already available, pending, or downloading",
			"Discover trending, upcoming, and popular titles",
			"Notifications by email, Telegram, Discord, or ntfy",
		},
		Screenshots: shots("jellyseerr", "The Jellyseerr home page: recently added and trending titles"),
		Logo:        logo("jellyseerr"), Developer: "Seerr team", License: "MIT",
		Website: "https://seerr.dev", Docs: "https://docs.seerr.dev",
	},

	"qbittorrent": {
		About: "qBittorrent is a torrent client with a full web interface. The arr apps hand it what to fetch; it downloads to the shared media folder, seeds for as long as you ask, and reports back so imports happen without you.\n\n" +
			"It is the client most guides assume, and its web UI does everything the desktop one does.",
		Highlights: []string{
			"A complete web interface",
			"Categories and per-category folders for the arr apps",
			"Bandwidth scheduling and seeding limits",
			"RSS with automatic downloading",
			"Sequential download and search plugins",
		},
		Screenshots: shots("qbittorrent", "The transfer list", "A torrent's details and peers", "Options"),
		Logo:        logo("qbittorrent"), Developer: "qBittorrent project", License: "GPL-2.0-or-later",
		Website: "https://www.qbittorrent.org", Docs: "https://github.com/qbittorrent/qBittorrent/wiki",
	},
	"sabnzbd": {
		About: "SABnzbd is a usenet downloader. Sonarr, Radarr, Lidarr, and Hook send it NZBs; it fetches, verifies, repairs, and unpacks the result into the shared folder, then tells them it is done.\n\n" +
			"It has run quietly for two decades, which is exactly what you want from a download client.",
		Highlights: []string{
			"Automatic verification, repair, and unpacking",
			"Categories with per-category folders and scripts",
			"Bandwidth scheduling and pause on demand",
			"A responsive interface with a night mode",
			"Fog whitelists its own hostname on install",
		},
		Screenshots: shots("sabnzbd", "The queue and history", "Night mode", "History details"),
		Logo:        logo("sabnzbd"), Developer: "The SABnzbd team", License: "GPL-2.0-or-later",
		Website: "https://sabnzbd.org", Docs: "https://sabnzbd.org/wiki/",
	},

	"jellyfin": {
		About: "Jellyfin is a media server: your films, series, and music, streamed to any screen in the house and beyond. Point it at the media folder and it fetches artwork and metadata, tracks what everyone has watched, and plays to the TV, the phone, and the browser.\n\n" +
			"It is the free and open alternative to Plex and Emby — no account with anyone, no features held back, no phoning home.",
		Highlights: []string{
			"Apps for every TV platform, phone, and browser",
			"Artwork, metadata, and subtitles fetched automatically",
			"Per-user libraries, parental controls, and watch history",
			"Hardware transcoding where the machine allows it",
			"Live TV and DVR with a tuner",
			"SyncPlay to watch together from different rooms",
		},
		Screenshots: shots("jellyfin", "The home screen", "A film's detail page", "A library", "Playback"),
		Logo:        logo("jellyfin"), Developer: "Jellyfin project", License: "GPL-2.0",
		Website: "https://jellyfin.org", Docs: "https://jellyfin.org/docs",
	},
	"navidrome": {
		About: "Navidrome streams your music collection. It scans the music folder, keeps up with additions, and plays through its own web player or any of the many apps that speak the Subsonic API — on the phone, in the car, on the desktop.\n\n" +
			"It is light enough to run on anything and fast with libraries of a hundred thousand tracks.",
		Highlights: []string{
			"Works with dozens of Subsonic-compatible apps",
			"Scans the folder in the background and picks up changes",
			"Playlists, smart playlists, favourites, and ratings per user",
			"Transcoding on the fly for slow links",
			"Last.fm and ListenBrainz scrobbling",
		},
		Screenshots: shots("navidrome", "The web player on a desktop", "The web player on a phone", "An album on a phone"),
		Logo:        logo("navidrome"), Developer: "Navidrome project", License: "GPL-3.0",
		Website: "https://www.navidrome.org", Docs: "https://www.navidrome.org/docs/",
	},
	"audiobookshelf": {
		About: "Audiobookshelf is a server for audiobooks and podcasts. It organises the library, keeps everyone's listening position, and plays through its own apps on iPhone and Android or in the browser — with downloads for offline listening.\n\n" +
			"Podcasts are fetched and kept on schedule, and metadata and covers come from the usual sources.",
		Highlights: []string{
			"Progress synced across the web and the phone apps",
			"Podcast subscriptions with automatic downloads",
			"Chapters, bookmarks, and playback speed",
			"Metadata and covers from Audible, iTunes, and more",
			"Multi-user with per-user progress and libraries",
			"Ebook reading too",
		},
		Screenshots: shots("audiobookshelf", "The library", "Streaming an audiobook"),
		Logo:        logo("audiobookshelf"), Developer: "advplyr", License: "GPL-3.0",
		Website: "https://www.audiobookshelf.org", Docs: "https://www.audiobookshelf.org/docs",
	},

	"uptime-kuma": {
		About: "Uptime Kuma watches the things you care about and tells you when they go down. Give it a URL, a host, a port, or a container, and it checks on an interval, keeps the history, and notifies you the moment something changes.\n\n" +
			"It also publishes status pages — a tidy public view of what is up for the household or for people you host things for.",
		Highlights: []string{
			"HTTP, TCP, ping, DNS, Docker, and keyword monitors",
			"Ninety notification services, ntfy among them",
			"Status pages with incidents and maintenance windows",
			"Response-time charts and certificate expiry warnings",
		},
		Screenshots: shots("uptime-kuma", "The dashboard in dark mode", "The dashboard in light mode"),
		Logo:        logo("uptime-kuma"), Developer: "Louis Lam", License: "MIT",
		Website: "https://uptime.kuma.pet", Docs: "https://github.com/louislam/uptime-kuma/wiki",
	},
	"vaultwarden": {
		About: "Vaultwarden is a password manager server that speaks Bitwarden's protocol. The official Bitwarden apps and browser extensions connect to it as if it were bitwarden.com — but the vault is here, encrypted with keys that never leave your devices.\n\n" +
			"It is written in Rust, needs a fraction of the resources of the upstream server, and includes the features Bitwarden charges for.",
		Highlights: []string{
			"Works with every official Bitwarden app and extension",
			"Organisations and collections for sharing within the household",
			"Two-factor sign-in, emergency access, and Send",
			"Admin page for users and settings",
		},
		Logo: logo("vaultwarden"), Developer: "Daniel García", License: "AGPL-3.0",
		Docs: "https://github.com/dani-garcia/vaultwarden/wiki",
	},
	"ntfy": {
		About: "ntfy is push notifications with nothing in the way. Publish a message to a topic with a single HTTP request — from a script, a cron job, or any of the apps here — and it arrives on your phone and desktop.\n\n" +
			"Feed, Hook, Uptime Kuma, and Jellyseerr can all be pointed at it, and Fog can send its own alerts here too.",
		Highlights: []string{
			"Publish with curl; receive in the iOS and Android apps or the browser",
			"Topics with access control",
			"Priorities, tags, attachments, and action buttons",
			"Scheduled delivery and email forwarding",
		},
		Screenshots: shots("ntfy", "The web app", "The Android app", "Publishing with curl"),
		Logo:        logo("ntfy"), Developer: "Philipp C. Heckel", License: "Apache-2.0 / GPL-2.0",
		Website: "https://ntfy.sh", Docs: "https://docs.ntfy.sh",
	},
	"adguard": {
		About: "AdGuard Home blocks ads and trackers for every device on the network at once. It runs as your DNS server: point the router at it and phones, TVs, and laptops stop resolving ad domains without any software installed on them.\n\n" +
			"It also gives you a view of what the devices in the house are talking to, and lets you block or allow per client.",
		Highlights: []string{
			"Network-wide blocking with no client software",
			"Per-client rules and parental controls",
			"Encrypted upstream DNS (DoH, DoT, DoQ)",
			"Query log and statistics",
			"Publishes port 53 on this machine",
		},
		Screenshots: shots("adguard", "The AdGuard Home dashboard"),
		Logo:        logo("adguard"), Developer: "AdGuard", License: "GPL-3.0",
		Website: "https://adguard.com/adguard-home/overview.html", Docs: "https://github.com/AdguardTeam/AdGuardHome/wiki",
	},
	"homeassistant": {
		About: "Home Assistant is the home-automation hub: lights, thermostats, locks, cameras, sensors, and media, from thousands of brands, brought under one roof and one interface, with automations that run locally.\n\n" +
			"Fog runs it on the container network, so integrations that discover devices by broadcast may need to be added by address instead.",
		Highlights: []string{
			"Thousands of integrations, most working locally",
			"Automations, scenes, and scripts with a visual editor",
			"Dashboards for phones, tablets, and wall panels",
			"Energy monitoring",
			"Voice assistants that stay in the house",
		},
		Screenshots: shots("homeassistant", "The overview dashboard", "Integrations"),
		Logo:        logo("homeassistant"), Developer: "Open Home Foundation", License: "Apache-2.0",
		Website: "https://www.home-assistant.io", Docs: "https://www.home-assistant.io/docs/",
	},
	"homepage": {
		About: "Homepage is a start page for everything running on this server: one tidy grid of the apps, with live status and a few numbers from each — downloads in progress, films requested, uptime — so a household can find its way around without bookmarks.\n\n" +
			"It is configured in YAML, and it reads directly from the apps it lists through their APIs.",
		Highlights: []string{
			"Widgets for a hundred-plus services, including everything in this store",
			"Docker integration: shows what is running",
			"Bookmarks, search, and weather",
			"Themes and backgrounds",
		},
		Screenshots: shots("homepage", "A dashboard in dark mode", "Homepage on a tablet", "A themed dashboard"),
		Logo:        logoWebp("homepage"), Developer: "Homepage community", License: "GPL-3.0",
		Website: "https://gethomepage.dev", Docs: "https://gethomepage.dev/configs/",
	},
	"gotenberg": {
		About: "Gotenberg is a document conversion service: an HTTP API in front of Chromium and LibreOffice that turns HTML, Markdown, Office files, and URLs into PDF, and converts between document formats.\n\n" +
			"It has no interface of its own. Office uses it for exact PDF export and for opening .doc, .odt, and .rtf; anything else on this machine can use it too.",
		Highlights: []string{
			"HTML, Markdown, and URLs to PDF through Chromium",
			"Word, Excel, PowerPoint, and OpenDocument to PDF through LibreOffice",
			"Merge, split, and PDF/A conversion",
			"Stateless: nothing to store, nothing to back up",
		},
		Logo: logoWebp("gotenberg"), Developer: "Gotenberg", License: "MIT",
		Website: "https://gotenberg.dev", Docs: "https://gotenberg.dev/docs/getting-started/introduction",
	},

	"gitea": {
		About: "Gitea is a git host with the trimmings: repositories, pull requests, issues, a package registry, and Actions, in a single light binary. It is where the household's own code lives — and the household's own apps — Hub, Fin, Office, Feed, Hook — deploy from a Gitea just like it.\n\n" +
			"Fog configures it from the environment on every boot, so the clone URLs it prints follow the address you give it.",
		Highlights: []string{
			"Pull requests, code review, and protected branches",
			"Issues, milestones, labels, and a wiki per repository",
			"Actions compatible with GitHub workflows",
			"Package registry for containers, npm, and more",
			"Clone over HTTPS at this app's address, or SSH on port 2222",
		},
		Screenshots: shots("gitea", "A repository", "A pull request", "Issues", "An Actions run"),
		Logo:        logo("gitea"), Developer: "Gitea", License: "MIT",
		Website: "https://about.gitea.com", Docs: "https://docs.gitea.com",
	},

	"vscode": {
		About: "VS Code, the editor, running on this server and used from a browser tab. Open it from a laptop, a tablet, or a borrowed machine and your files, settings, extensions, and terminal are exactly where you left them — the work happens here, not on whatever you're typing on.\n\n" +
			"It is code-server, the open-source build of VS Code that Coder packages for the web. Extensions install from Open VSX, so most of the usual ones are there; a few Microsoft-only ones are not.",
		Highlights: []string{
			"The full VS Code editor, with its settings, themes, and keybindings",
			"An integrated terminal on the server itself",
			"Extensions from the Open VSX marketplace",
			"Git built in: stage, commit, and push from the sidebar",
			"Everything under one volume, so backups carry your whole setup",
		},
		Screenshots: shots("vscode", "Editing a file with the explorer open", "The welcome page"),
		Logo:        logo("vscode"), Developer: "Coder", License: "MIT",
		Website: "https://coder.com/docs/code-server", Docs: "https://coder.com/docs/code-server/guide",
	},

	"n8n": {
		About: "n8n is a workflow automation tool: you draw a flow of nodes on a canvas — a trigger, then steps that fetch, transform, decide, and send — and it runs whenever the trigger fires. A webhook, a schedule, a new email, or a message in a chat can start one, and its 400-odd integrations reach most services a household uses.\n\n" +
			"When a node doesn't exist for what you need, a Code node takes JavaScript or Python, and an HTTP Request node talks to any API. AI agent nodes put a language model in the middle of a flow, with your other nodes as its tools.",
		Highlights: []string{
			"A visual editor for flows, with every run's data inspectable step by step",
			"Triggers from webhooks, schedules, email, and hundreds of services",
			"Code nodes in JavaScript or Python when a built-in node isn't enough",
			"AI agents that call your workflows and integrations as tools",
			"Credentials encrypted at rest in Fog's Postgres",
		},
		Screenshots: shots("n8n", "A workflow that drafts email replies with an AI model"),
		Logo:        logo("n8n"), Developer: "n8n", License: "Sustainable Use License",
		Website: "https://n8n.io", Docs: "https://docs.n8n.io",
	},
}
