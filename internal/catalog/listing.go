package catalog

// The store front. An Entry's spec says how an app runs; its listing says
// what it is, in the terms a person choosing between two apps thinks in —
// the paragraph, the screenshots, who makes it and under what licence. The
// listing is display copy only: nothing here reaches a container.
//
// Artwork lives beside the entry in its catalog (apps/<id>/) and fog serves
// it from its own copy of the catalog, so a store page never fetches from
// the internet — a Fog box on a LAN with no route out still gets its logos.
// In app.yaml, logo and screenshot sources are file names in the entry's
// folder; loaded, they are the URLs fog serves them at (AssetURL).

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
	// Logo is the app's mark: a square image the UI draws a rounded tile
	// around — unless the file is named tile.<ext>, which is drawn edge to
	// edge with its tile already part of the picture.
	Logo      string `json:"logo,omitempty"`
	Developer string `json:"developer,omitempty"`
	License   string `json:"license,omitempty"`
	// Website is the project's front door; Docs its manual. Either may be
	// empty; ProjectURL on the entry is the source repository.
	Website string `json:"website,omitempty"`
	Docs    string `json:"docs,omitempty"`
}
