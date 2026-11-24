package terminal

// history is the normal screen's lines that scrolled off its top, oldest first, within a
// budget of bytes: the oldest go when it's full. Each is kept encoded, as a terminal would be
// sent it (see encodeLine), with the rows a long line was wrapped into joined, so a client
// wraps it at its own width.
//
// The lines are packed into chunks, and where each is kept in blocks, so that even a flood
// of short lines costs few allocations and nothing is ever copied to grow. Chunks and blocks
// are freed from the front as their lines go. The budget counts both.
type history struct {
	limit     int // bytes; 0 keeps none
	size      int
	chunkSize int

	chunks     [][]byte
	firstChunk uint32 // the number of chunks[0]: lines refer to chunks by number
	blocks     [][]historyLine
	head, n    int // the lines are from blocks[0][head], n of them

	// open is the last line continuing in the next one pushed.
	open bool
	// dropped counts the lines evicted, and evicted is the offset of the last of them.
	dropped uint64
	evicted uint64
}

type historyLine struct {
	// offset is the session's output offset that scrolled the line, or its last row, off.
	offset     uint64
	chunk      uint32
	start, end uint32
}

const (
	maxHistoryChunk = 64 << 10
	historyBlock    = 1024
	// historyLineCost is what a line's place in a block costs.
	historyLineCost = 24
)

func newHistory(limit int) *history {
	limit = max(limit, 0)
	// Small enough for a small budget to hold several.
	return &history{limit: limit, chunkSize: min(maxHistoryChunk, max(limit/16, 512))}
}

func (h *history) line(i int) *historyLine {
	i += h.head
	return &h.blocks[i/historyBlock][i%historyBlock]
}

func (h *history) data(l *historyLine) []byte {
	return h.chunks[l.chunk-h.firstChunk][l.start:l.end]
}

// push adds a row that scrolled off at offset. wrapped means the line goes on in the next
// row pushed.
func (h *history) push(data []byte, wrapped bool, offset uint64) {
	if h.limit == 0 {
		return
	}
	if h.open && h.n > 0 {
		last := h.line(h.n - 1)
		if chunk := h.chunks[len(h.chunks)-1]; cap(chunk)-len(chunk) >= len(data) {
			h.chunks[len(h.chunks)-1] = append(chunk, data...)
			last.end += uint32(len(data)) //nolint:gosec // bounded by the chunk
		} else {
			// It moves to a new chunk, with room to go on.
			previous := h.data(last)
			at := h.reserve(len(previous) + len(data))
			chunk := append(append(h.chunks[len(h.chunks)-1], previous...), data...)
			h.chunks[len(h.chunks)-1] = chunk
			last.chunk, last.start, last.end = h.lastChunk(), uint32(at), uint32(len(chunk)) //nolint:gosec // bounded
		}
		last.offset = offset
	} else {
		at := h.reserve(len(data))
		chunk := append(h.chunks[len(h.chunks)-1], data...)
		h.chunks[len(h.chunks)-1] = chunk
		h.append(historyLine{offset: offset, chunk: h.lastChunk(), start: uint32(at), //nolint:gosec // bounded
			end: uint32(len(chunk))}) //nolint:gosec // bounded
	}
	h.open = wrapped
	h.evict()
}

func (h *history) lastChunk() uint32 {
	return h.firstChunk + uint32(len(h.chunks)) - 1 //nolint:gosec // as many as the budget holds
}

// reserve makes room for n bytes at the end of the last chunk, and returns where they go.
func (h *history) reserve(n int) int {
	if len(h.chunks) > 0 {
		if last := h.chunks[len(h.chunks)-1]; cap(last)-len(last) >= n {
			return len(last)
		}
	}
	size := max(n, h.chunkSize)
	h.chunks = append(h.chunks, make([]byte, 0, size))
	h.size += size
	return 0
}

func (h *history) append(l historyLine) {
	if (h.head+h.n)/historyBlock == len(h.blocks) {
		h.blocks = append(h.blocks, make([]historyLine, historyBlock))
	}
	*h.line(h.n) = l
	h.n++
	h.size += historyLineCost
}

// evict drops the oldest lines, and the chunks and blocks they leave empty, until the
// history is in its budget.
func (h *history) evict() {
	for h.size > h.limit && h.n > 0 {
		gone := h.line(0)
		h.dropped++
		h.evicted = gone.offset
		h.head++
		h.n--
		h.size -= historyLineCost
		if h.head == historyBlock || h.n == 0 {
			h.blocks[0] = nil
			h.blocks = h.blocks[1:]
			h.head = 0
		}
		for len(h.chunks) > 0 && (h.n == 0 || h.line(0).chunk > h.firstChunk) {
			h.size -= cap(h.chunks[0])
			h.chunks[0] = nil
			h.chunks = h.chunks[1:]
			h.firstChunk++
		}
	}
	if h.n == 0 {
		h.open = false
		h.blocks = nil
	}
}

// clear forgets the history, as a program asked (ED 3). That's not a gap.
func (h *history) clear() {
	h.firstChunk += uint32(len(h.chunks)) //nolint:gosec // as many as the budget holds
	h.chunks, h.blocks, h.head, h.n, h.size, h.open = nil, nil, 0, 0, 0, false
}

// since returns the lines scrolled off after offset, and whether some of those are gone.
func (h *history) since(offset uint64) ([][]byte, bool) {
	i := h.n
	for i > 0 && h.line(i-1).offset > offset {
		i--
	}
	return h.from(i), h.dropped > 0 && h.evicted > offset
}

// all returns every line kept, and whether some are gone.
func (h *history) all() ([][]byte, bool) {
	return h.from(0), h.dropped > 0
}

func (h *history) from(i int) [][]byte {
	lines := make([][]byte, 0, h.n-i)
	for ; i < h.n; i++ {
		lines = append(lines, h.data(h.line(i)))
	}
	return lines
}
