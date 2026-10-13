package retriever

import (
	"fmt"
	"unicode"
)

// TextChunk records rune offsets into the original content. Boundaries are
// selected only between extended grapheme-like clusters so combining marks,
// variation selectors, emoji modifiers, and ZWJ sequences stay intact.
type TextChunk struct {
	Content   string
	StartRune int
	EndRune   int
}

type graphemeCluster struct {
	runes []rune
	start int
	end   int
}

func ChunkText(content string, chunkSize, overlap int) ([]TextChunk, error) {
	if chunkSize <= 0 {
		return nil, fmt.Errorf("chunk_size must be positive")
	}
	if overlap < 0 || overlap >= chunkSize {
		return nil, fmt.Errorf("chunk_overlap must be non-negative and less than chunk_size")
	}
	clusters := splitGraphemeLikeClusters([]rune(content))
	if len(clusters) == 0 {
		return []TextChunk{}, nil
	}

	chunks := make([]TextChunk, 0, (len([]rune(content))/maxInt(1, chunkSize-overlap))+1)
	startCluster := 0
	for startCluster < len(clusters) {
		endCluster := startCluster
		used := 0
		for endCluster < len(clusters) {
			clusterLen := len(clusters[endCluster].runes)
			if endCluster > startCluster && used+clusterLen > chunkSize {
				break
			}
			used += clusterLen
			endCluster++
			if used >= chunkSize {
				break
			}
		}
		if endCluster == startCluster {
			endCluster++
		}
		chunkRunes := make([]rune, 0, used)
		for _, cluster := range clusters[startCluster:endCluster] {
			chunkRunes = append(chunkRunes, cluster.runes...)
		}
		chunks = append(chunks, TextChunk{
			Content:   string(chunkRunes),
			StartRune: clusters[startCluster].start,
			EndRune:   clusters[endCluster-1].end,
		})
		if endCluster == len(clusters) {
			break
		}

		// Move forward while retaining at most overlap runes of whole clusters.
		nextStart := endCluster
		kept := 0
		for nextStart > startCluster {
			clusterLen := len(clusters[nextStart-1].runes)
			if kept+clusterLen > overlap {
				break
			}
			kept += clusterLen
			nextStart--
		}
		if nextStart <= startCluster {
			nextStart = startCluster + 1
		}
		startCluster = nextStart
	}
	return chunks, nil
}

func splitGraphemeLikeClusters(input []rune) []graphemeCluster {
	clusters := make([]graphemeCluster, 0, len(input))
	for index := 0; index < len(input); {
		start := index
		cluster := []rune{input[index]}
		regionalCount := 0
		if isRegionalIndicator(input[index]) {
			regionalCount = 1
		}
		index++
		for index < len(input) {
			r := input[index]
			if isCombiningOrModifier(r) {
				cluster = append(cluster, r)
				index++
				continue
			}
			if r == '\u200d' && index+1 < len(input) {
				cluster = append(cluster, r, input[index+1])
				index += 2
				continue
			}
			if regionalCount == 1 && isRegionalIndicator(r) {
				cluster = append(cluster, r)
				index++
			}
			break
		}
		clusters = append(clusters, graphemeCluster{runes: cluster, start: start, end: index})
	}
	return clusters
}

func isCombiningOrModifier(r rune) bool {
	return unicode.Is(unicode.Mn, r) || unicode.Is(unicode.Mc, r) || unicode.Is(unicode.Me, r) ||
		(r >= '\ufe00' && r <= '\ufe0f') || (r >= '\U000e0100' && r <= '\U000e01ef') ||
		(r >= '\U0001f3fb' && r <= '\U0001f3ff')
}

func isRegionalIndicator(r rune) bool {
	return r >= '\U0001f1e6' && r <= '\U0001f1ff'
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
