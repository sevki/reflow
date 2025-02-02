// Copyright 2017 GRAIL, Inc. All rights reserved.
// Use of this source code is governed by the Apache 2.0
// license that can be found in the LICENSE file.

package local

import (
	"context"
	"crypto/md5"
	"expvar"
	"fmt"
	"math/rand"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go/aws/awserr"

	"github.com/aws/aws-sdk-go/service/s3"

	"github.com/grailbio/reflow"
	"github.com/grailbio/reflow/blob"
	"github.com/grailbio/reflow/blob/s3blob"
	"github.com/grailbio/reflow/errors"
	"github.com/grailbio/reflow/repository/filerepo"
	reflowtestutil "github.com/grailbio/reflow/test/testutil"
	"github.com/grailbio/testutil"
	"github.com/grailbio/testutil/s3test"
)

type testStore map[string]blob.Bucket

func (s testStore) Bucket(ctx context.Context, name string) (blob.Bucket, error) {
	bucket, ok := s[name]
	if !ok {
		return nil, errors.E("testStore.Bucket", name, errors.NotExist)
	}
	return bucket, nil
}

func newS3Test(t *testing.T, bucket, prefix string, transferType string) (exec *blobExec, client *s3test.Client, repo *filerepo.Repository, cleanup func()) {
	var dir string
	dir, cleanup = testutil.TempDir(t, "", "s3test")
	repo = &filerepo.Repository{Root: filepath.Join(dir, "repo")}
	client = s3test.NewClient(t, bucket)
	client.Region = "us-west-2"
	store := testStore{"testbucket": s3blob.NewBucket("testbucket", client)}
	exec = &blobExec{
		Blob:         blob.Mux{"s3": store},
		Repository:   repo,
		Root:         filepath.Join(dir, "exec"),
		ExecID:       reflow.Digester.FromString("s3test"),
		transferType: transferType,
	}
	if transferType == intern {
		exec.staging.Root = filepath.Join(dir, "staging")
	}
	exec.Config = reflow.ExecConfig{
		Type: transferType,
		URL:  "s3://" + bucket + "/" + prefix,
	}
	exec.Init(nil)
	return
}

func executeAndGetResult(ctx context.Context, t *testing.T, s3 *blobExec) reflow.Result {
	t.Helper()
	go s3.Go(ctx)
	if err := s3.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	inspect, err := s3.Inspect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := inspect.Error; err != nil {
		t.Fatalf("unexpected error %v", err)
	}
	if got, want := inspect.State, "complete"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}

	res, err := s3.Result(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

type file struct {
	path, sha256 string
}

func getFile(path string, withSha bool) file {
	f := file{path: path}
	if withSha {
		f.sha256 = reflow.Digester.FromString(path).String()
	}
	return f
}

func TestS3ExecInternPrefix(t *testing.T) {
	const (
		bucket = "testbucket"
		prefix = "prefix/"
	)
	s3x, client, repo, cleanup := newS3Test(t, bucket, prefix, intern)
	defer cleanup()

	inRepoFile := getFile("already/in/repo", true)
	if _, err := repo.Put(context.Background(), strings.NewReader(inRepoFile.path)); err != nil {
		t.Fatal(err)
	}
	files := []file{
		getFile("a", true),
		getFile("a/b", true),
		getFile("d", true),
		getFile("d/e/f/g", false),
		getFile("abcdefg", false),
		inRepoFile,
	}
	client.Err = func(api string, input interface{}) error {
		if api != "GetObjectRequest" {
			return nil
		}
		if goi, ok := input.(*s3.GetObjectInput); ok {
			if strings.HasSuffix(*goi.Key, inRepoFile.path) {
				return awserr.New("Unexpected", "GetObject should not be called on key already in repo", nil)
			}
		}
		return nil
	}
	val := reflow.Fileset{
		Map: map[string]reflow.File{},
	}
	for _, file := range files {
		client.SetFile(prefix+file.path, []byte(file.path), file.sha256)
		// Get the file to access the LastModified which is set as time.Now() by the test client.
		fc, _ := client.GetFile(prefix + file.path)
		rf := reflow.File{
			ID:           reflow.Digester.FromString(file.path),
			Source:       fmt.Sprintf("s3://%s/%s%s", bucket, prefix, file.path),
			ETag:         fmt.Sprintf("%x", md5.Sum([]byte(file.path))),
			LastModified: fc.LastModified,
			Size:         int64(len(file.path)),
		}
		rf.Assertions = blob.Assertions(rf)
		val.Map[file.path] = rf
	}
	ctx := context.Background()
	res2 := executeAndGetResult(ctx, t, s3x)

	if got, want := res2, (reflow.Result{Fileset: val}); !got.Equal(want) {
		t.Errorf("got %v, want %v", got, want)
	}
	if err := s3x.Promote(ctx); err != nil {
		t.Fatal(err)
	}

	// Verify that everything is in the repository.
	for _, file := range val.Map {
		ok, err := repo.Contains(file.ID)
		if err != nil {
			t.Error(err)
		}
		if !ok {
			t.Errorf("repo is missing %v", file.ID)
		}
	}
	// Compare assertions
	for path := range res2.Fileset.Map {
		if got, want := res2.Fileset.Map[path].Assertions, val.Map[path].Assertions; !got.Equal(want) {
			t.Errorf("got %v, want %v", got, want)
		}
	}
}

func TestS3ExecExternPrefix(t *testing.T) {
	const (
		bucket = "testbucket"
		prefix = "prefix/"
	)
	s3, client, repo, cleanup := newS3Test(t, bucket, prefix, extern)
	defer cleanup()

	files := []string{"a", "a/b", "d", "d/e/f/g", "abcdefg"}
	fileset := reflowtestutil.WriteFiles(repo, files...)
	s3.Config.Args = []reflow.Arg{{Fileset: &fileset}}

	ctx := context.Background()
	res := executeAndGetResult(ctx, t, s3)

	if got, want := res, (reflow.Result{Fileset: fileset}); !got.Equal(want) {
		t.Errorf("got %v, want %v", got, want)
	}

	// Verify that everything is in the blob.
	for _, file := range files {
		content, ok := client.GetFile(prefix + file)
		if !ok {
			t.Errorf("blob is missing %v", file)
		}
		if content.Content.Size() != int64(len(file)) {
			t.Errorf("blob content mismatch %v", file)
		}
	}
}

func TestS3ExecExternFileFileset(t *testing.T) {
	const (
		bucket = "testbucket"
		prefix = "filename"
	)
	s3, client, repo, cleanup := newS3Test(t, bucket, prefix, extern)
	defer cleanup()

	contents := "abcdefg"
	file := reflowtestutil.WriteFile(repo, contents)
	fileset := reflow.Fileset{
		Map: map[string]reflow.File{
			".": file,
		},
	}
	s3.Config.Args = []reflow.Arg{{Fileset: &fileset}}

	ctx := context.Background()
	res := executeAndGetResult(ctx, t, s3)

	if got, want := res, (reflow.Result{Fileset: fileset}); !got.Equal(want) {
		t.Errorf("got %v, want %v", got, want)
	}

	// Verify that everything is in the blob.
	content, ok := client.GetFile(prefix)
	if !ok {
		t.Errorf("blob is missing %v, filename %v", file, prefix)
	}
	if content.Content.Size() != int64(len(contents)) {
		t.Errorf("blob content mismatch %v", file)
	}
}

func TestS3ExecPath(t *testing.T) {
	const (
		bucket   = "testbucket"
		key      = "somefile"
		contents = "file contents"
	)
	s3, client, repo, cleanup := newS3Test(t, bucket, key, intern)
	defer cleanup()

	client.SetFile(key, []byte(contents), "unused")
	client.SetFile(key+"suffix", []byte(contents), "unused")
	client.SetFile("someotherfile", []byte("blah"), "unused")

	ctx := context.Background()
	got := executeAndGetResult(ctx, t, s3)
	if err := s3.Promote(ctx); err != nil {
		t.Fatal(err)
	}

	// Get the file to access the LastModified which is set as time.Now() by the test client.
	fc, _ := client.GetFile(key)
	rf := reflow.File{
		ID:           reflow.Digester.FromString(contents),
		Source:       fmt.Sprintf("s3://%s/%s", bucket, key),
		ETag:         fmt.Sprintf("%x", md5.Sum([]byte(contents))),
		LastModified: fc.LastModified,
		Size:         int64(len(contents)),
	}
	rf.Assertions = blob.Assertions(rf)

	want := reflow.Result{Fileset: reflow.Fileset{
		Map: map[string]reflow.File{".": rf},
	}}
	if !got.Equal(want) {
		t.Errorf("got %v, want %v", got, want)
	}
	ok, err := repo.Contains(want.Fileset.Map["."].ID)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Errorf("file is missing from repository")
	}
}

func TestRateWatch(t *testing.T) {
	numRws, numLoops := 5+rand.Intn(10), 5+rand.Intn(10)
	exp := expvar.NewInt("hello")
	rws := make([]*rateExporter, numRws)
	for i := 0; i < numRws; i++ {
		rws[i] = newRateExporter(exp)
	}
	for j := 0; j < numLoops; j++ {
		time.Sleep(time.Duration(rand.Intn(50)) * time.Millisecond)
		for i := 0; i < numRws; i++ {
			rws[i].Add(rand.Int63n(10<<20) + 1<<20)
		}
		sum := int64(0)
		for i := 0; i < numRws; i++ {
			sum += rws[i].rate()
		}
		if got, want := exp.Value(), sum; got != want {
			t.Errorf("got %d, want %d", got, want)
		}
	}
}
