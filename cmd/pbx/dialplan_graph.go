package main

// Inbound dial plan — a directed graph the operator edits visually in the
// console. Each inbound trunk call walks the graph from the `start` node:
// `match` nodes branch on trunk + dialed number (DID); action nodes ring an
// extension, hand off to the IVR, forward externally, play audio/TTS, or reject.

// Node types.
const (
	dpStart      = "start"
	dpMatch      = "match"
	dpExt        = "ext"
	dpIVR        = "ivr"
	dpReject     = "reject"
	dpForward    = "forward"
	dpPlay       = "play"
	dpTTS        = "tts"
	dpAnswerNode = "answer"
	dpGatherNode = "gather"
	dpWaitNode   = "wait"
)

// dpPortDefault is the gather output taken when the entry matches no option
// (or on timeout / no input).
const dpPortDefault = "default"

// Gather input modes.
const (
	dpInputDTMF   = "dtmf"
	dpInputSpeech = "speech"
	dpInputBoth   = "both"
)

// Edge port names.
const (
	dpPortOut      = "out"      // start → next
	dpPortMatch    = "match"    // match node, condition true
	dpPortNoMatch  = "nomatch"  // match node, condition false
	dpPortNext     = "next"     // play/tts/answer/wait → next
	dpPortNoAnswer = "noanswer" // ext → taken when nobody answers (timeout/offline/declined)
)

// DID match modes on a `match` node.
const (
	didAny    = "any"
	didExact  = "exact"
	didPrefix = "prefix"
	didRegex  = "regex"
)

// DPNode is one node in the flow. Params holds type-specific fields (see the
// per-type notes in the engine): match → {trunk, did_mode, did}; ext → {number};
// reject → {reason}; forward → {number, trunk}; play → {url}; tts → {text, voice}.
type DPNode struct {
	ID   string `json:"id"`
	Type string `json:"type"`
	// X/Y are canvas coordinates for the editor only (unused by the engine).
	// float64 so the JSON decoder accepts fractional positions (HiDPI scroll).
	X      float64           `json:"x"`
	Y      float64           `json:"y"`
	Params map[string]string `json:"params,omitempty"`
}

// DPEdge connects a node's output port to another node's input.
type DPEdge struct {
	From string `json:"from"`
	Port string `json:"port"`
	To   string `json:"to"`
}

// DPGraph is the whole dial plan.
type DPGraph struct {
	Nodes []DPNode `json:"nodes"`
	Edges []DPEdge `json:"edges"`
}

// node returns the node with the given id, or nil.
func (g DPGraph) node(id string) *DPNode {
	for i := range g.Nodes {
		if g.Nodes[i].ID == id {
			return &g.Nodes[i]
		}
	}
	return nil
}

// start returns the single start node, or nil.
func (g DPGraph) start() *DPNode {
	for i := range g.Nodes {
		if g.Nodes[i].Type == dpStart {
			return &g.Nodes[i]
		}
	}
	return nil
}

// edgeTo returns the target node id of the edge leaving fromID via port, or "".
func (g DPGraph) edgeTo(fromID, port string) string {
	for _, e := range g.Edges {
		if e.From == fromID && e.Port == port {
			return e.To
		}
	}
	return ""
}

// param reads a node param with a default.
func (n *DPNode) param(key, def string) string {
	if n == nil || n.Params == nil {
		return def
	}
	if v, ok := n.Params[key]; ok && v != "" {
		return v
	}
	return def
}

// defaultDialplan is seeded on first run: every inbound call → the IVR. This
// restores (and makes explicit) the previous hard-coded behaviour while giving
// the operator a graph to edit, and ensures inbound trunk calls are never
// silently rejected out of the box.
func defaultDialplan() DPGraph {
	return DPGraph{
		Nodes: []DPNode{
			{ID: "start", Type: dpStart, X: 60, Y: 60},
			{ID: "ivr", Type: dpIVR, X: 320, Y: 60},
		},
		Edges: []DPEdge{
			{From: "start", Port: dpPortOut, To: "ivr"},
		},
	}
}
