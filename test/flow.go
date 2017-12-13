// Copyright 2017 GRAIL, Inc. All rights reserved.
// Use of this source code is governed by the Apache 2.0
// license that can be found in the LICENSE file.

package test

import (
	"bytes"
	"context"
	"fmt"
	"strings"

	"github.com/grailbio/reflow"
)

// The following are useful constructors for testing.

// File returns a file object representing the given contents.
func File(contents string) reflow.File {
	return reflow.File{
		reflow.Digester.FromString(contents),
		int64(len(contents)),
	}
}

// Files returns a value comprising the given files with contents derived from
// their names.
func Files(files ...string) reflow.Fileset {
	var v reflow.Fileset
	v.Map = map[string]reflow.File{}
	for _, file := range files {
		var path, contents string
		parts := strings.SplitN(file, ":", 2)
		switch len(parts) {
		case 1:
			path = file
			contents = file
		case 2:
			path = parts[0]
			contents = parts[1]
		}
		v.Map[path] = File(contents)
	}
	return v
}

// WriteFiles writes the provided files into the repository r and
// returns a Fileset as in Files.
func WriteFiles(r reflow.Repository, files ...string) reflow.Fileset {
	for _, file := range files {
		_, err := r.Put(context.Background(), bytes.NewReader([]byte(file)))
		if err != nil {
			panic(fmt.Sprintf("unexpected error writing to repository: %v", err))
		}
	}
	return Files(files...)
}

// List constructs a list value.
func List(values ...reflow.Fileset) reflow.Fileset {
	return reflow.Fileset{List: values}
}
