package topicgraph

// EvaluationData is private input for an aggregate graph evaluation. Its
// version and seed identifiers are only used for bounded local traversal and
// must never be included in reports, logs, or transport output.
type EvaluationData struct {
	Available              bool
	ActiveVersionID        string
	EligibleDocuments      int
	MentionDocuments       int
	Mentions               int
	RelationCandidates     int
	Edges                  int
	Episodes               int
	EpisodeMemberships     int
	RelationTypes          map[RelationType]int
	RelationConfidences    []float64
	Warnings               map[string]int
	TimelineMentionSeedIDs []string
}
