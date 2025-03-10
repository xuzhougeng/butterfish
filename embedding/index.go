package embedding

import (
	"context"
	"fmt"
	"io"
	"io/ioutil"
	"mime"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	pb "github.com/xuzhougeng/butterfish/proto"
	"github.com/drewlanenga/govector"
	"github.com/golang/protobuf/proto"
	"github.com/spf13/afero"
	fsutil "golang.org/x/tools/godoc/util"
	"golang.org/x/tools/godoc/vfs"
	"google.golang.org/protobuf/types/known/timestamppb"

	util "github.com/xuzhougeng/butterfish/util"
)

type Embedder interface {
	CalculateEmbeddings(ctx context.Context, content []string) ([][]float32, error)
}

type FileEmbeddingIndex interface {
	SetEmbedder(embedder Embedder)
	Search(ctx context.Context, query string, numResults int) ([]*VectorSearchResult, error)
	Vectorize(ctx context.Context, content string) ([]float32, error)
	SearchWithVector(ctx context.Context, queryVector []float32, k int) ([]*VectorSearchResult, error)
	PopulateSearchResults(ctx context.Context, embeddings []*VectorSearchResult) error
	ClearPaths(ctx context.Context, paths []string) error
	ClearPath(ctx context.Context, path string) error
	LoadPaths(ctx context.Context, paths []string) error
	LoadPath(ctx context.Context, path string) error
	IndexPaths(ctx context.Context, paths []string, forceUpdate bool, chunkSize, maxChunks int) error
	IndexPath(ctx context.Context, path string, forceUpdate bool, chunkSize, maxChunks int) error
	IndexedFiles() []string
}

type VectorSearchResult struct {
	Score    float64
	FilePath string
	Start    uint64
	End      uint64
	Vector   []float32
	Content  string
}

type DiskCachedEmbeddingIndex struct {
	// maps absolute path of directory to a directory Index
	Index map[string]*pb.DirectoryIndex

	// Interface to an Embedder used to embed chunks of documents
	Embedder Embedder

	// A filesystem interface, used when reading and writing files.
	// We use an interface here so that we can mock the filesystem during testing.
	Fs afero.Fs

	// The output stream to use for logging
	Out io.Writer

	// The Verbosity level of the output stream
	// 0 - no output
	// 1 - most important calls
	// 2 - more detail about embeddings
	Verbosity int

	// The name of the file to cache the index on disk
	DotfileName string

	// When we call the embedder we batch chunks together into a single call,
	// this is the number of chunks to batch together
	ChunksPerCall int

	// When we embed a path we skip these directories
	IgnoreDirs []string

	// When we embed a path we skip these files
	IgnoreFiles []string
}

func NewDiskCachedEmbeddingIndex(embedder Embedder, writer io.Writer) *DiskCachedEmbeddingIndex {
	index := &DiskCachedEmbeddingIndex{
		Embedder:    embedder,
		Index:       make(map[string]*pb.DirectoryIndex),
		Out:         writer,
		Fs:          afero.NewOsFs(),
		IgnoreDirs:  []string{".git"},
		IgnoreFiles: []string{".gitignore", ".gitmodules", "go.sum", "LICENSE", "LICENSE.md"},
	}

	index.SetDefaultConfig()
	return index
}

func (this *DiskCachedEmbeddingIndex) SetDefaultConfig() {
	this.DotfileName = ".butterfish_index"
	this.ChunksPerCall = 32
}

func (this *DiskCachedEmbeddingIndex) SetEmbedder(embedder Embedder) {
	this.Embedder = embedder
}

func (this *DiskCachedEmbeddingIndex) SetOutput(out io.Writer) {
	this.Out = out
	this.Verbosity = 2
}

func (this *DiskCachedEmbeddingIndex) SetVerbosity(verbosity int) {
	this.Verbosity = verbosity
}

// Search the vectors that have been loaded into memory by embedding the
// query string and then searching for the closest vectors based on a cosine
// distance. This method calls the following methods in succession.
// 1. Vectorize()
// 2. SearchWithVector()
// 3. PopulateSearchResults()
func (this *DiskCachedEmbeddingIndex) Search(ctx context.Context, query string, numResults int) ([]*VectorSearchResult, error) {
	queryVector, err := this.Vectorize(ctx, query)
	if err != nil {
		return nil, err
	}

	results, err := this.SearchWithVector(ctx, queryVector, numResults)
	if err != nil {
		return nil, err
	}

	err = this.PopulateSearchResults(ctx, results)
	if err != nil {
		return nil, err
	}

	return results, nil
}

// Vectorize the given string by embedding it with the current embedder.
func (this *DiskCachedEmbeddingIndex) Vectorize(ctx context.Context, content string) ([]float32, error) {
	if this.Embedder == nil {
		return nil, fmt.Errorf("no embedder set")
	}

	embeddings, err := this.Embedder.CalculateEmbeddings(ctx, []string{content})
	if err != nil {
		return nil, err
	}

	return embeddings[0], nil
}

// Super naive vector search operation.
// - First we brute force search by iterating over all stored vectors
//     and calculating cosine distance
// - Next we sort based on score
func (this *DiskCachedEmbeddingIndex) SearchWithVector(ctx context.Context,
	queryVector []float32, numResults int) ([]*VectorSearchResult, error) {
	// Turn queryVector float array into a govector
	query, err := govector.AsVector(queryVector)
	if err != nil {
		return nil, err
	}

	results := []*VectorSearchResult{}

	for dirIndexAbsPath, dirIndex := range this.Index {
		for filename, fileIndex := range dirIndex.Files {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}

			for _, embedding := range fileIndex.Embeddings {
				govec, err := govector.AsVector(embedding.Vector)

				distance, err := govector.Cosine(query, govec)
				if err != nil {
					return nil, err
				}

				absPath := filepath.Join(dirIndexAbsPath, filename)
				result := &VectorSearchResult{
					Score:    distance,
					FilePath: absPath,
					Start:    embedding.Start,
					End:      embedding.End,
					Vector:   embedding.Vector,
				}
				results = append(results, result)
			}
		}
	}

	sort.Slice(results, func(i, j int) bool {
		return results[i].Score > results[j].Score
	})

	// truncate to numResults results
	results = results[:util.Min(len(results), numResults)]

	return results, nil
}

// Given an array of VectorSearchResults, fetch the file contents for each
// result and store it in the result's Content field.
func (this *DiskCachedEmbeddingIndex) PopulateSearchResults(ctx context.Context,
	results []*VectorSearchResult) error {

	for _, result := range results {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		// read the file
		f, err := this.Fs.Open(result.FilePath)
		if err != nil {
			return err
		}
		defer f.Close()

		start := result.Start
		end := result.End

		// seek to the start byte
		_, err = f.Seek(int64(start), 0)
		if err != nil {
			return err
		}

		// read the chunk
		buf := make([]byte, end-start)
		_, err = f.Read(buf)
		if err != nil {
			return err
		}

		result.Content = string(buf)
	}

	return nil
}

// Assumes the path is a valid butterfish index file
func (this *DiskCachedEmbeddingIndex) LoadDotfile(dotfile string) error {
	dotfile = filepath.Clean(dotfile)

	if this.Verbosity >= 2 {
		fmt.Fprintf(this.Out, "DiskCachedEmbeddingIndex.LoadDotfile(%s)\n", dotfile)
	}

	// Read the entire dotfile into a bytes buffer
	file, err := this.Fs.Open(dotfile)
	if err != nil {
		return nil
	}
	defer file.Close()

	// Read the entire file into a buffer
	buf, err := ioutil.ReadAll(file)
	if err != nil {
		return err
	}

	// Unmarshal the buffer into a DirectoryIndex
	var dirIndex pb.DirectoryIndex
	err = proto.Unmarshal(buf, &dirIndex)
	if err != nil {
		return err
	}

	absPath, err := filepath.Abs(dotfile)
	if err != nil {
		return err
	}
	indexName := filepath.Dir(absPath)

	// put the loaded info in the memory index
	this.Index[indexName] = &dirIndex

	if this.Verbosity >= 1 {
		fmt.Fprintf(this.Out, "Loaded index cache at %s\n", dotfile)
	}
	return nil
}

func (this *DiskCachedEmbeddingIndex) SavePaths(paths []string) error {
	for _, path := range paths {
		err := this.SavePath(path)
		if err != nil {
			return err
		}
	}
	return nil
}

func (this *DiskCachedEmbeddingIndex) SavePath(path string) error {
	if this.Verbosity >= 2 {
		fmt.Fprintf(this.Out, "DiskCachedEmbeddingIndex.SavePath(%s)\n", path)
	}

	path = filepath.Clean(path)

	// Marshal the index into a buffer, i.e. serialize in-memory protobuf
	// to the byte representation
	dirIndex, ok := this.Index[path]
	if !ok {
		return fmt.Errorf("No index found for %s", path)
	}

	buf, err := proto.Marshal(dirIndex)
	if err != nil {
		return err
	}

	if this.DotfileName == "" {
		panic("DotfileName not set")
	}

	dotfilePath := filepath.Join(path, this.DotfileName)
	if this.Verbosity >= 2 {
		fmt.Fprintf(this.Out, "Writing index cache to %s\n", dotfilePath)
	}

	// Write the buffer to the dotfile
	err = afero.WriteFile(this.Fs, dotfilePath, buf, 0644)
	if err != nil {
		return err
	}

	if this.Verbosity >= 1 {
		fmt.Fprintf(this.Out, "Saved index cache to %s\n", dotfilePath)
	}
	return nil
}

func (this *DiskCachedEmbeddingIndex) LoadPath(ctx context.Context, path string) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}

	if this.Verbosity >= 2 {
		fmt.Fprintf(this.Out, "DiskCachedEmbeddingIndex.Load(%s)\n", path)
	}

	// Check the path exists, bail out if not
	path = filepath.Clean(path)
	fileInfo, err := this.Fs.Stat(path)
	if err != nil {
		return err
	}

	// If the path is a file then find its parent directory
	dirPath := path
	if !fileInfo.IsDir() {
		dirPath = filepath.Dir(path)
	}

	dotfiles, err := this.dotfilesInPath(ctx, dirPath)
	if err != nil {
		return err
	}

	for _, dotfile := range dotfiles {
		err := this.LoadDotfile(dotfile)
		if err != nil {
			return err
		}
	}
	return nil
}

func (this *DiskCachedEmbeddingIndex) LoadPaths(ctx context.Context, paths []string) error {
	for _, path := range paths {
		err := this.LoadPath(ctx, path)
		if err != nil {
			return err
		}
	}

	return nil
}

func (this *DiskCachedEmbeddingIndex) IndexPaths(ctx context.Context, paths []string, forceUpdate bool, chunkSize, maxChunks int) error {
	for _, path := range paths {
		err := this.IndexPath(ctx, path, forceUpdate, chunkSize, maxChunks)
		if err != nil {
			return err
		}
	}

	return nil
}

// This is a bit of glue to make afero filesystems work with the vfs interface
type vfsOpener struct {
	fs afero.Fs
}

func (this *vfsOpener) Open(path string) (vfs.ReadSeekCloser, error) {
	return this.fs.Open(path)
}

// Return true if this is a file we want to index/embed. We use several
// predicates to determine this.
// 1. The file must be a non-hidden file (i.e. not starting with a dot)
// 2. The file must not be a directory (handled separately)
// 3. The file must be text, not binary, checked by extension/mime-type and
//    by checking the first few bytes of the file if the extension check passes
// 4. The file must have been updated since the last indexing, unless forceUpdate is true
func (this *DiskCachedEmbeddingIndex) IndexableFile(path string, file os.FileInfo, forceUpdate bool, previousEmbeddings *pb.FileEmbeddings) bool {
	// Ignore dotfiles/hidden files
	name := file.Name()
	if name[0] == '.' {
		return false
	}

	// Ignore files that are not text based on file name
	mimeType := mime.TypeByExtension(filepath.Ext(name))
	if mimeType != "" && !strings.HasPrefix(mimeType, "text/") {
		return false
	}

	// Ignore files in the disallow list
	if contains(this.IgnoreFiles, name) {
		return false
	}

	// Ignore files that are not text based on a content check
	opener := &vfsOpener{this.Fs}
	if !fsutil.IsTextFile(opener, filepath.Join(path, name)) {
		return false
	}

	if !forceUpdate && previousEmbeddings != nil {
		// Ignore files that have not changed since the last indexing
		if previousEmbeddings.UpdatedAt.AsTime().Unix() >= file.ModTime().Unix() {
			return false
		}
	}

	return true
}

// check if a string array contains a string
func contains(strs []string, str string) bool {
	for _, s := range strs {
		if s == str {
			return true
		}
	}
	return false
}

func (this *DiskCachedEmbeddingIndex) IndexableDirectory(path string) bool {
	name := filepath.Base(path)

	if contains(this.IgnoreDirs, name) {
		return false
	}
	return true
}

func (this *DiskCachedEmbeddingIndex) FilterUnindexablefiles(path string, files []os.FileInfo, forceUpdate bool, dirIndex *pb.DirectoryIndex) []os.FileInfo {
	var filteredFiles []os.FileInfo
	for _, file := range files {
		if file.IsDir() {
			continue
		}

		previousEmbeddings := dirIndex.Files[file.Name()]
		if this.IndexableFile(path, file, forceUpdate, previousEmbeddings) {
			filteredFiles = append(filteredFiles, file)
		} else {
			fmt.Fprintf(this.Out, "Ignored %s\n", filepath.Join(path, file.Name()))
		}
	}
	return filteredFiles
}

func (this *DiskCachedEmbeddingIndex) dotfilesInPath(ctx context.Context, path string) ([]string, error) {
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}

	dotfiles := []string{}

	// Use Walk to search recursively for dotfiles
	err := afero.Walk(this.Fs, path, func(path string, info os.FileInfo, err error) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil {
			return err
		}

		if info.Name() == this.DotfileName {
			dotfiles = append(dotfiles, path)
		}
		return nil
	})

	return dotfiles, err
}

func (this *DiskCachedEmbeddingIndex) ClearPaths(ctx context.Context, paths []string) error {
	for _, path := range paths {
		err := this.ClearPath(ctx, path)
		if err != nil {
			return err
		}
	}
	return nil
}

// Clear out embeddings at a given path, both in memory and on disk
// We do this by first locating all dotfiles in the path, then deleting
// the in-memory copy, and finally deleting the dotfiles
func (this *DiskCachedEmbeddingIndex) ClearPath(ctx context.Context, path string) error {
	path, err := filepath.Abs(path)
	if err != nil {
		return err
	}

	dotfiles, err := this.dotfilesInPath(ctx, path)
	if err != nil {
		return err
	}

	for _, dotfile := range dotfiles {
		if this.Verbosity >= 2 {
			fmt.Fprintf(this.Out, "Removing dotfile %s\n", dotfile)
		}

		err = this.Fs.Remove(dotfile)
		if err != nil {
			return err
		}

		// Remove the in-memory copy
		dirPath := filepath.Dir(dotfile)
		delete(this.Index, dirPath)
	}

	return nil
}

func (this *DiskCachedEmbeddingIndex) IndexedFiles() []string {
	var paths []string
	for path, dirIndex := range this.Index {
		for name := range dirIndex.Files {
			paths = append(paths, filepath.Join(path, name))
		}
	}
	return paths
}

func NewDirectoryIndex() *pb.DirectoryIndex {
	return &pb.DirectoryIndex{
		Files: make(map[string]*pb.FileEmbeddings),
	}
}

// Force means that we will re-index the file even if the target file hasn't
// changed since the last index
func (this *DiskCachedEmbeddingIndex) IndexPath(ctx context.Context, path string, forceUpdate bool, chunkSize, maxChunks int) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}

	if this.Verbosity >= 2 {
		fmt.Fprintf(this.Out, "DiskCachedEmbeddingIndex.IndexPath(%s)\n", path)
	}

	path, err := filepath.Abs(path)
	if err != nil {
		return err
	}

	fileInfo, err := this.Fs.Stat(path)
	if err != nil {
		return err
	}

	var files []os.FileInfo
	var dirPath string

	if !fileInfo.IsDir() {
		// if the path is a specific file then we only update that file
		dirPath = filepath.Dir(path)
		files = []os.FileInfo{fileInfo}
	} else {
		// if the path is a directory then we add all files to update list
		dirPath = path

		// call UpdatePath recursively for each subdirectory
		err = util.ForEachSubdir(this.Fs, path, func(path string) error {
			if this.IndexableDirectory(path) {
				return this.IndexPath(ctx, path, forceUpdate, chunkSize, maxChunks)
			}

			fmt.Fprintf(this.Out, "Ignored %s\n", path)
			return nil
		})

		// get each non-directory file and stat in the path
		files, err = afero.ReadDir(this.Fs, path)
		if err != nil {
			return nil
		}
	}

	// Fetch directory index, create a new one if none found
	dirIndex, ok := this.Index[dirPath]
	if !ok {
		dirIndex = NewDirectoryIndex()
		this.Index[dirPath] = dirIndex
	}

	files = this.FilterUnindexablefiles(dirPath, files, forceUpdate, dirIndex)

	// Update the index for each file
	for _, file := range files {
		name := file.Name()
		path := filepath.Join(dirPath, file.Name())
		fileEmbeddings, err := this.EmbedFile(ctx, path, chunkSize, maxChunks)
		if err != nil {
			return err
		}

		dirIndex.Files[name] = fileEmbeddings
		fmt.Fprintf(this.Out, "Indexed %s\n", path)
	}

	// TODO remove indexes for files that have been deleted

	if len(dirIndex.Files) > 0 {
		return this.SavePath(dirPath)
	}

	return nil
}

// EmbedFile takes a path to a file, splits the file into chunks, and calls
// the embedding API for each chunk
func (this *DiskCachedEmbeddingIndex) EmbedFile(ctx context.Context, path string, chunkSize, maxChunks int) (*pb.FileEmbeddings, error) {
	if this.Embedder == nil {
		return nil, fmt.Errorf("No embedder set")
	}
	if this.Verbosity >= 1 {
		fmt.Fprintf(this.Out, "Embedding %s\n", path)
	}

	annotatedVectors := []*pb.AnnotatedEmbedding{}

	absPath, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	timestamp := time.Now()

	if chunkSize == 0 {
		return nil, fmt.Errorf("Chunk size must be greater than 0")
	}

	// first we chunk the file
	chunks, err := util.GetFileChunks(ctx, this.Fs, absPath, chunkSize, maxChunks)
	if err != nil {
		return nil, err
	}
	stringChunks := util.ByteToString(chunks)

	// then we call the embedding API for each block of chunks
	for i := 0; i < len(chunks); i += this.ChunksPerCall {
		// check if we should bail out
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}

		callChunks := stringChunks[i:util.Min(i+this.ChunksPerCall, len(chunks))]
		newEmbeddings, err := this.Embedder.CalculateEmbeddings(ctx, callChunks)
		if err != nil {
			return nil, err
		}

		// iterate through response, create an annotation, and create an annotated vector
		for j, embedding := range newEmbeddings {
			rangeStart := uint64(i+j) * uint64(chunkSize)
			rangeEnd := rangeStart + uint64(len(callChunks[j]))

			av := &pb.AnnotatedEmbedding{
				Start:  rangeStart,
				End:    rangeEnd,
				Vector: embedding,
			}
			annotatedVectors = append(annotatedVectors, av)
		}
	}

	fileEmbeddings := &pb.FileEmbeddings{
		Path:       filepath.Base(absPath),
		UpdatedAt:  timestamppb.New(timestamp),
		Embeddings: annotatedVectors,
	}

	return fileEmbeddings, nil
}
