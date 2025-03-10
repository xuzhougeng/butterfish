package embedding

import (
	"context"
	"os"
	"testing"

	pb "github.com/xuzhougeng/butterfish/proto"
	"github.com/spf13/afero"
	"github.com/stretchr/testify/assert"
)

// A basic check to make sure vector comparisons are working
func TestSearch(t *testing.T) {
	index := &DiskCachedEmbeddingIndex{
		Index: map[string]*pb.DirectoryIndex{
			"/path/foo": {
				Files: map[string]*pb.FileEmbeddings{
					"test.txt": {
						Embeddings: []*pb.AnnotatedEmbedding{
							{
								Start:  0,
								End:    1,
								Vector: []float32{1, 0, 0, 0, 0},
							},
							{
								Start:  1,
								End:    2,
								Vector: []float32{0, 1, 0, 0, 0},
							},
							{
								Start:  2,
								End:    3,
								Vector: []float32{0, 0, 1, 0, 0},
							},
							{
								Start:  3,
								End:    4,
								Vector: []float32{0, 0, 0, 1, 0},
							},
							{
								Start:  4,
								End:    5,
								Vector: []float32{0, 0, 0, 0, 1},
							},
						},
					},
				},
			},
		},
	}

	results, err := index.SearchWithVector(context.Background(), []float32{1, 0.5, 0, 0, 0}, 3)
	assert.NoError(t, err)
	assert.Equal(t, 3, len(results))
	assert.Equal(t, "/path/foo/test.txt", results[0].FilePath)
	// The first and second vectors should be the closest matches
	assert.Equal(t, uint64(0), results[0].Start)
}

// A mock embedder that implements the Embedder interface
type mockEmbedder struct {
	Calls int
}

func (this *mockEmbedder) CalculateEmbeddings(ctx context.Context, content []string) ([][]float32, error) {
	embeddings := make([][]float32, len(content))
	for i, str := range content {
		// create a fake embedding of the ascii values of the first 5 chars
		embeddings[i] = make([]float32, 128)
		embeddings[i][int(str[0])] = 1
	}

	this.Calls++

	return embeddings, nil
}

func makeFakeFilesystem(t *testing.T) afero.Fs {
	appFS := afero.NewMemMapFs()
	// create test files and directories
	err := appFS.MkdirAll("/a", 0755)
	assert.NoError(t, err)
	err = afero.WriteFile(appFS, "/a/one", []byte("111111"), 0644)
	assert.NoError(t, err)
	err = afero.WriteFile(appFS, "/a/two", []byte("222222"), 0644)
	assert.NoError(t, err)
	err = afero.WriteFile(appFS, "/a/b/nine", []byte("999999"), 0644)
	assert.NoError(t, err)
	err = afero.WriteFile(appFS, "/a/b/c/d/four", []byte("444444"), 0644)
	assert.NoError(t, err)
	return appFS
}

func newTestDiskCachedEmbeddingIndex(fs afero.Fs) (*DiskCachedEmbeddingIndex, *mockEmbedder) {

	embedder := &mockEmbedder{}

	vectorIndex := &DiskCachedEmbeddingIndex{
		Index:     map[string]*pb.DirectoryIndex{},
		Embedder:  embedder,
		Out:       os.Stdout,
		Verbosity: 2,
		Fs:        fs,
	}
	vectorIndex.SetDefaultConfig()

	return vectorIndex, embedder
}

// The goal here is to test index caching on disk, we use a mock filesystem
// and mock out the embedding function
func TestFileCaching(t *testing.T) {
	fs := makeFakeFilesystem(t)
	index, embedder := newTestDiskCachedEmbeddingIndex(fs)
	ctx := context.Background()

	// index files in /a/b/c, this should only find "four"
	err := index.IndexPath(ctx, "/a/b/c", false, 512, 8)
	assert.NoError(t, err)
	assert.Equal(t, 1, embedder.Calls)

	scored, err := index.Search(ctx, "444", 3)
	assert.NoError(t, err)
	assert.Equal(t, 1, len(scored))
	assert.Equal(t, "/a/b/c/d/four", scored[0].FilePath)

	// New index, we should be able to load the cached index and search again
	index, embedder = newTestDiskCachedEmbeddingIndex(fs)
	err = index.LoadPath(ctx, "/a/b/c")
	assert.NoError(t, err)

	indexed := index.IndexedFiles()
	assert.Equal(t, 1, len(indexed))
	assert.Equal(t, "/a/b/c/d/four", indexed[0])

	scored, err = index.Search(ctx, "444", 3)
	assert.NoError(t, err)
	assert.Equal(t, 1, len(scored))
	assert.Equal(t, "/a/b/c/d/four", scored[0].FilePath)

	// Index the same path, we should not have to re-embed
	assert.Equal(t, 1, embedder.Calls) // this is 1 because search calls embedder
	err = index.IndexPath(ctx, "/a/b/c", false, 512, 8)
	assert.NoError(t, err)
	assert.Equal(t, 1, embedder.Calls)

	// Index everything, we should end up with more dotfiles written
	err = index.IndexPath(ctx, "/a", false, 512, 8)
	assert.NoError(t, err)
	exists, err := afero.Exists(fs, "/a/.butterfish_index")
	assert.NoError(t, err)
	assert.True(t, exists)
	exists, err = afero.Exists(fs, "/a/b/.butterfish_index")
	assert.NoError(t, err)
	assert.True(t, exists)

	// Try searching now
	scored, err = index.Search(ctx, "222222", 2)
	assert.NoError(t, err)
	assert.Equal(t, 2, len(scored))
	assert.Equal(t, "/a/two", scored[0].FilePath)

	// Try out clearing the "four" file
	index.ClearPath(ctx, "/a/b/c")
	scored, err = index.Search(ctx, "444", 2)
	assert.NoError(t, err)
	assert.NotEqual(t, "/a/b/c/d/four", scored[0].FilePath)
	exists, err = afero.Exists(fs, "/a/b/c/d/.butterfish_index")
	assert.NoError(t, err)
	assert.False(t, exists)

	// TODO test showindexed
}
