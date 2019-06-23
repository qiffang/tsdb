// Copyright 2017 The Prometheus Authors
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package tsdb

import (
	"context"
	"encoding/binary"
	"errors"
	"github.com/prometheus/tsdb/labels"
	"github.com/prometheus/tsdb/testutil/fuse"
	"io/ioutil"
	"math/rand"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/go-kit/kit/log"
	"github.com/prometheus/tsdb/chunks"
	"github.com/prometheus/tsdb/testutil"
	"github.com/prometheus/tsdb/tsdbutil"
)

// In Prometheus 2.1.0 we had a bug where the meta.json version was falsely bumped
// to 2. We had a migration in place resetting it to 1 but we should move immediately to
// version 3 next time to avoid confusion and issues.
func TestBlockMetaMustNeverBeVersion2(t *testing.T) {
	dir, err := ioutil.TempDir("", "metaversion")
	testutil.Ok(t, err)
	defer func() {
		testutil.Ok(t, os.RemoveAll(dir))
	}()

	testutil.Ok(t, writeMetaFile(log.NewNopLogger(), dir, &BlockMeta{}))

	meta, err := readMetaFile(dir)
	testutil.Ok(t, err)
	testutil.Assert(t, meta.Version != 2, "meta.json version must never be 2")
}

func TestSetCompactionFailed(t *testing.T) {
	tmpdir, err := ioutil.TempDir("", "test")
	testutil.Ok(t, err)
	defer func() {
		testutil.Ok(t, os.RemoveAll(tmpdir))
	}()

	blockDir := createBlock(t, tmpdir, genSeries(1, 1, 0, 0))
	b, err := OpenBlock(nil, blockDir, nil)
	testutil.Ok(t, err)
	testutil.Equals(t, false, b.meta.Compaction.Failed)
	testutil.Ok(t, b.setCompactionFailed())
	testutil.Equals(t, true, b.meta.Compaction.Failed)
	testutil.Ok(t, b.Close())

	b, err = OpenBlock(nil, blockDir, nil)
	testutil.Ok(t, err)
	testutil.Equals(t, true, b.meta.Compaction.Failed)
	testutil.Ok(t, b.Close())
}

func TestCreateBlock(t *testing.T) {
	tmpdir, err := ioutil.TempDir("", "test")
	testutil.Ok(t, err)
	defer func() {
		testutil.Ok(t, os.RemoveAll(tmpdir))
	}()
	b, err := OpenBlock(nil, createBlock(t, tmpdir, genSeries(1, 1, 0, 10)), nil)
	if err == nil {
		testutil.Ok(t, b.Close())
	}
	testutil.Ok(t, err)
}

func TestCorruptedChunk(t *testing.T) {
	for name, test := range map[string]struct {
		corrFunc func(f *os.File) // Func that applies the corruption.
		expErr   error
	}{
		"invalid header size": {
			func(f *os.File) {
				err := f.Truncate(1)
				testutil.Ok(t, err)
			},
			errors.New("invalid chunk header in segment 0: invalid size"),
		},
		"invalid magic number": {
			func(f *os.File) {
				magicChunksOffset := int64(0)
				_, err := f.Seek(magicChunksOffset, 0)
				testutil.Ok(t, err)

				// Set invalid magic number.
				b := make([]byte, chunks.MagicChunksSize)
				binary.BigEndian.PutUint32(b[:chunks.MagicChunksSize], 0x00000000)
				n, err := f.Write(b)
				testutil.Ok(t, err)
				testutil.Equals(t, chunks.MagicChunksSize, n)
			},
			errors.New("invalid magic number 0"),
		},
		"invalid chunk format version": {
			func(f *os.File) {
				chunksFormatVersionOffset := int64(4)
				_, err := f.Seek(chunksFormatVersionOffset, 0)
				testutil.Ok(t, err)

				// Set invalid chunk format version.
				b := make([]byte, chunks.ChunksFormatVersionSize)
				b[0] = 0
				n, err := f.Write(b)
				testutil.Ok(t, err)
				testutil.Equals(t, chunks.ChunksFormatVersionSize, n)
			},
			errors.New("invalid chunk format version 0"),
		},
	} {
		t.Run(name, func(t *testing.T) {
			tmpdir, err := ioutil.TempDir("", "test_open_block_chunk_corrupted")
			testutil.Ok(t, err)
			defer func() {
				testutil.Ok(t, os.RemoveAll(tmpdir))
			}()

			blockDir := createBlock(t, tmpdir, genSeries(1, 1, 0, 0))
			files, err := sequenceFiles(chunkDir(blockDir))
			testutil.Ok(t, err)
			testutil.Assert(t, len(files) > 0, "No chunk created.")

			f, err := os.OpenFile(files[0], os.O_RDWR, 0666)
			testutil.Ok(t, err)

			// Apply corruption function.
			test.corrFunc(f)
			testutil.Ok(t, f.Close())

			_, err = OpenBlock(nil, blockDir, nil)
			testutil.Equals(t, test.expErr.Error(), err.Error())
		})
	}
}

// createBlock creates a block with given set of series and returns its dir.
func createBlock(tb testing.TB, dir string, series []Series) string {
	head := createHead(tb, series)
	compactor, err := NewLeveledCompactor(context.Background(), nil, log.NewNopLogger(), []int64{1000000}, nil)
	testutil.Ok(tb, err)

	testutil.Ok(tb, os.MkdirAll(dir, 0777))

	ulid, err := compactor.Write(dir, head, head.MinTime(), head.MaxTime(), nil)
	testutil.Ok(tb, err)
	return filepath.Join(dir, ulid.String())
}

func createHead(tb testing.TB, series []Series) *Head {
	head, err := NewHead(nil, nil, nil, 2*60*60*1000)
	testutil.Ok(tb, err)
	defer head.Close()

	app := head.Appender()
	for _, s := range series {
		ref := uint64(0)
		it := s.Iterator()
		for it.Next() {
			t, v := it.At()
			if ref != 0 {
				err := app.AddFast(ref, t, v)
				if err == nil {
					continue
				}
			}
			ref, err = app.Add(s.Labels(), t, v)
			testutil.Ok(tb, err)
		}
		testutil.Ok(tb, it.Err())
	}
	err = app.Commit()
	testutil.Ok(tb, err)
	return head
}

const (
	defaultLabelName  = "labelName"
	defaultLabelValue = "labelValue"
)

// genSeries generates series with a given number of labels and values.
func genSeries(totalSeries, labelCount int, mint, maxt int64) []Series {
	if totalSeries == 0 || labelCount == 0 {
		return nil
	}

	series := make([]Series, totalSeries)

	for i := 0; i < totalSeries; i++ {
		lbls := make(map[string]string, labelCount)
		lbls[defaultLabelName] = strconv.Itoa(i)
		for j := 1; len(lbls) < labelCount; j++ {
			lbls[defaultLabelName+strconv.Itoa(j)] = defaultLabelValue + strconv.Itoa(j)
		}
		samples := make([]tsdbutil.Sample, 0, maxt-mint+1)
		for t := mint; t <= maxt; t++ {
			samples = append(samples, sample{t: t, v: rand.Float64()})
		}
		series[i] = newSeries(lbls, samples)
	}
	return series
}

// populateSeries generates series from given labels, mint and maxt.
func populateSeries(lbls []map[string]string, mint, maxt int64) []Series {
	if len(lbls) == 0 {
		return nil
	}

	series := make([]Series, 0, len(lbls))
	for _, lbl := range lbls {
		if len(lbl) == 0 {
			continue
		}
		samples := make([]tsdbutil.Sample, 0, maxt-mint+1)
		for t := mint; t <= maxt; t++ {
			samples = append(samples, sample{t: t, v: rand.Float64()})
		}
		series = append(series, newSeries(lbl, samples))
	}
	return series
}

// TestFailedDelete ensures that the block is in its original state when a delete fails.
func TestFailedDelete(t *testing.T) {
	original, err := ioutil.TempDir("", "original")
	testutil.Ok(t, err)
	mountpoint, err := ioutil.TempDir("", "mountpoint")
	testutil.Ok(t, err)

	defer func() {
		testutil.Ok(t, os.RemoveAll(mountpoint))
		testutil.Ok(t, os.RemoveAll(original))
	}()

	_, file := filepath.Split(createBlock(t, original, genSeries(1, 1, 1, 100)))
	server, err := fuse.NewServer(original, mountpoint, fuse.FailingRenameHook{})
	if err != nil {
		t.Skip(err) // Skip the test for any error. These tests are optional
	}
	defer func() {
		testutil.Ok(t, server.Close())
	}()

	expHash, err := testutil.DirHash(original)
	testutil.Ok(t, err)
	pb, err := OpenBlock(nil, filepath.Join(mountpoint, file), nil)
	testutil.Ok(t, err)
	defer func() {
		testutil.Ok(t, pb.Close())
	}()

	testutil.NotOk(t, pb.Delete(1, 10, labels.NewMustRegexpMatcher("", ".*")))

	actHash, err := testutil.DirHash(original)
	testutil.Ok(t, err)

	testutil.Equals(t, expHash, actHash, "the block dir hash has changed after a failed delete")
}
