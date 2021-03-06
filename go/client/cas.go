package client

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"sort"
	"sync"

	"github.com/bazelbuild/remote-apis-sdks/go/digest"
	log "github.com/golang/glog"
	"github.com/golang/protobuf/proto"
	"github.com/pborman/uuid"
	"golang.org/x/sync/errgroup"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
)

// WriteBlobs stores a large number of blobs from a digest-to-blob map. It's intended for use on the
// result of PackageTree. Unlike with the single-item functions, it first queries the CAS to
// see which blobs are missing and only uploads those that are.
func (c *Client) WriteBlobs(ctx context.Context, blobs map[digest.Key][]byte) error {
	if c.casConcurrency <= 0 {
		return fmt.Errorf("CASConcurrency should be at least 1")
	}
	const (
		logInterval = 25
	)

	var dgs []*repb.Digest
	for k := range blobs {
		dgs = append(dgs, digest.FromKey(k))
	}

	missing, err := c.MissingBlobs(ctx, dgs)
	if err != nil {
		return err
	}
	log.V(1).Infof("%d blobs to store", len(missing))
	var batches [][]*repb.Digest
	if c.useBatchOps {
		batches = makeBatches(missing)
	} else {
		log.V(1).Info("uploading them individually")
		for i := range missing {
			batches = append(batches, missing[i:i+1])
		}
	}

	eg, eCtx := errgroup.WithContext(ctx)
	todo := make(chan []*repb.Digest, c.casConcurrency)
	for i := 0; i < int(c.casConcurrency) && i < len(batches); i++ {
		eg.Go(func() error {
			for batch := range todo {
				if len(batch) > 1 {
					log.V(2).Infof("uploading batch of %d blobs", len(batch))
					bchMap := make(map[digest.Key][]byte)
					for _, dg := range batch {
						bchMap[digest.ToKey(dg)] = blobs[digest.ToKey(dg)]
					}
					if err := c.BatchWriteBlobs(eCtx, bchMap); err != nil {
						return err
					}
				} else {
					log.V(2).Info("uploading single blob")
					if _, err := c.WriteBlob(eCtx, blobs[digest.ToKey(batch[0])]); err != nil {
						return err
					}
				}
				if eCtx.Err() != nil {
					return eCtx.Err()
				}
			}
			return nil
		})
	}

	for len(batches) > 0 {
		select {
		case todo <- batches[0]:
			batches = batches[1:]
			if len(batches)%logInterval == 0 {
				log.V(1).Infof("%d batches left to store", len(batches))
			}
		case <-eCtx.Done():
			close(todo)
			return eCtx.Err()
		}
	}
	close(todo)
	log.V(1).Info("Waiting for remaining jobs")
	err = eg.Wait()
	log.V(1).Info("Done")
	return err
}

// WriteProto marshals and writes a proto.
func (c *Client) WriteProto(ctx context.Context, msg proto.Message) (*repb.Digest, error) {
	bytes, err := proto.Marshal(msg)
	if err != nil {
		return nil, err
	}
	return c.WriteBlob(ctx, bytes)
}

// WriteBlob uploads a blob to the CAS.
func (c *Client) WriteBlob(ctx context.Context, blob []byte) (*repb.Digest, error) {
	dg := digest.FromBlob(blob)
	name := c.ResourceNameWrite(dg.Hash, dg.SizeBytes)
	if err := c.WriteBytes(ctx, name, blob); err != nil {
		return nil, err
	}
	return dg, nil
}

const (
	// MaxBatchSz is the maximum size of a batch to upload with BatchWriteBlobs. We set it to slightly
	// below 4 MB, because that is the limit of a message size in gRPC
	MaxBatchSz = 4*1024*1024 - 1024

	// MaxBatchDigests is a suggested approximate limit based on current RBE implementation.
	// Above that BatchUpdateBlobs calls start to exceed a typical minute timeout.
	MaxBatchDigests = 4000
)

// BatchWriteBlobs uploads a number of blobs to the CAS. They must collectively be below the
// maximum total size for a batch upload, which is about 4 MB (see MaxBatchSz). Digests must be
// computed in advance by the caller. In case multiple errors occur during the blob upload, the
// last error will be returned.
func (c *Client) BatchWriteBlobs(ctx context.Context, blobs map[digest.Key][]byte) error {
	var reqs []*repb.BatchUpdateBlobsRequest_Request
	var sz int64
	for k, b := range blobs {
		dg := digest.FromKey(k)
		sz += dg.SizeBytes
		reqs = append(reqs, &repb.BatchUpdateBlobsRequest_Request{
			Digest: dg,
			Data:   b,
		})
	}
	if sz > MaxBatchSz {
		return fmt.Errorf("batch update of %d total bytes exceeds maximum of %d", sz, MaxBatchSz)
	}
	if len(blobs) > MaxBatchDigests {
		return fmt.Errorf("batch update of %d total blobs exceeds maximum of %d", len(blobs), MaxBatchDigests)
	}
	closure := func() error {
		var resp *repb.BatchUpdateBlobsResponse
		err := c.callWithTimeout(ctx, func(ctx context.Context) (e error) {
			resp, e = c.cas.BatchUpdateBlobs(ctx, &repb.BatchUpdateBlobsRequest{
				InstanceName: c.InstanceName,
				Requests:     reqs,
			})
			return e
		})
		if err != nil {
			return err
		}

		numErrs, errDg, errMsg := 0, new(repb.Digest), ""
		var failedReqs []*repb.BatchUpdateBlobsRequest_Request
		var retriableError error
		allRetriable := true
		for _, r := range resp.Responses {
			st := status.FromProto(r.Status)
			if st.Code() != codes.OK {
				e := st.Err()
				if c.retrier.ShouldRetry(e) {
					failedReqs = append(failedReqs, &repb.BatchUpdateBlobsRequest_Request{
						Digest: r.Digest,
						Data:   blobs[digest.ToKey(r.Digest)],
					})
					retriableError = e
				} else {
					allRetriable = false
				}
				numErrs++
				errDg = r.Digest
				errMsg = r.Status.Message
			}
		}
		reqs = failedReqs
		if numErrs > 0 {
			if allRetriable {
				return retriableError // Retriable errors only, retry the failed requests.
			}
			return fmt.Errorf("uploading blobs as part of a batch resulted in %d failures, including blob %s: %s", numErrs, digest.ToString(errDg), errMsg)
		}
		return nil
	}
	return c.retrier.do(ctx, closure)
}

// makeBatches splits a list of digests into batches of size no more than the maximum.
//
// First, we sort all the blobs, then we make each batch by taking the largest available blob and
// then filling in with as many small blobs as we can fit. This is a naive approach to the knapsack
// problem, and may have suboptimal results in some cases, but it results in deterministic batches,
// runs in O(n log n) time, and avoids most of the pathological cases that result from scanning from
// one end of the list only.
//
// The input list is sorted in-place; additionally, any blob bigger than the maximum will be put in
// a batch of its own and the caller will need to ensure that it is uploaded with Write, not batch
// operations.
func makeBatches(dgs []*repb.Digest) [][]*repb.Digest {
	var batches [][]*repb.Digest
	log.V(1).Infof("Batching %d digests", len(dgs))
	sort.Slice(dgs, func(i, j int) bool {
		return dgs[i].SizeBytes < dgs[j].SizeBytes
	})
	for len(dgs) > 0 {
		batch := []*repb.Digest{dgs[len(dgs)-1]}
		dgs = dgs[:len(dgs)-1]
		sz := batch[0].SizeBytes
		for len(dgs) > 0 && len(batch) < MaxBatchDigests && dgs[0].SizeBytes <= MaxBatchSz-sz { // dg.SizeBytes+sz possibly overflows so subtract instead.
			sz += dgs[0].SizeBytes
			batch = append(batch, dgs[0])
			dgs = dgs[1:]
		}
		log.V(2).Infof("created batch of %d blobs with total size %d", len(batch), sz)
		batches = append(batches, batch)
	}
	log.V(1).Infof("%d batches created", len(batches))
	return batches
}

// ReadBlob fetches a blob from the CAS into a byte slice.
func (c *Client) ReadBlob(ctx context.Context, d *repb.Digest) ([]byte, error) {
	return c.readBlob(ctx, d.Hash, d.SizeBytes, 0, 0)
}

// ReadBlobRange fetches a partial blob from the CAS into a byte slice, starting from offset bytes
// and including at most limit bytes (or no limit if limit==0). The offset must be non-negative and
// no greater than the size of the entire blob. The limit must not be negative, but offset+limit may
// be greater than the size of the entire blob.
func (c *Client) ReadBlobRange(ctx context.Context, d *repb.Digest, offset, limit int64) ([]byte, error) {
	return c.readBlob(ctx, d.Hash, d.SizeBytes, offset, limit)
}

func (c *Client) readBlob(ctx context.Context, hash string, sizeBytes, offset, limit int64) ([]byte, error) {
	// int might be 32-bit, in which case we could have a blob whose size is representable in int64
	// but not int32, and thus can't fit in a slice. We can check for this by casting and seeing if
	// the result is negative, since 32 bits is big enough wrap all out-of-range values of int64 to
	// negative numbers. If int is 64-bits, the cast is a no-op and so the condition will always fail.
	if int(sizeBytes) < 0 {
		return nil, fmt.Errorf("digest size %d is too big to fit in a byte slice", sizeBytes)
	}
	if offset > sizeBytes {
		return nil, fmt.Errorf("offset %d out of range for a blob of size %d", offset, sizeBytes)
	}
	if offset < 0 {
		return nil, fmt.Errorf("offset %d may not be negative", offset)
	}
	if limit < 0 {
		return nil, fmt.Errorf("limit %d may not be negative", limit)
	}
	sz := sizeBytes - offset
	if limit > 0 && limit < sz {
		sz = limit
	}
	sz += bytes.MinRead // Pad size so bytes.Buffer does not reallocate.
	buf := bytes.NewBuffer(make([]byte, 0, sz))
	_, err := c.readBlobStreamed(ctx, hash, sizeBytes, offset, limit, buf)
	return buf.Bytes(), err
}

// ReadBlobToFile fetches a blob with a provided digest name from the CAS, saving it into a file.
// It returns the number of bytes read.
func (c *Client) ReadBlobToFile(ctx context.Context, d *repb.Digest, fpath string) (int64, error) {
	return c.readBlobToFile(ctx, d.Hash, d.SizeBytes, fpath)
}

func (c *Client) readBlobToFile(ctx context.Context, hash string, sizeBytes int64, fpath string) (int64, error) {
	n, err := c.readToFile(ctx, c.resourceNameRead(hash, sizeBytes), fpath)
	if err != nil {
		return n, err
	}
	if n != sizeBytes {
		return n, fmt.Errorf("CAS fetch read %d bytes but %d were expected", n, sizeBytes)
	}
	return n, nil
}

// ReadBlobStreamed fetches a blob with a provided digest from the CAS.
// It streams into an io.Writer, and returns the number of bytes read.
func (c *Client) ReadBlobStreamed(ctx context.Context, d *repb.Digest, w io.Writer) (int64, error) {
	return c.readBlobStreamed(ctx, d.Hash, d.SizeBytes, 0, 0, w)
}

func (c *Client) readBlobStreamed(ctx context.Context, hash string, sizeBytes, offset, limit int64, w io.Writer) (int64, error) {
	n, err := c.readStreamed(ctx, c.resourceNameRead(hash, sizeBytes), offset, limit, w)
	if err != nil {
		return n, err
	}
	sz := sizeBytes - offset
	if limit > 0 && limit < sz {
		sz = limit
	}
	if n != sz {
		return n, fmt.Errorf("CAS fetch read %d bytes but %d were expected", n, sz)
	}
	return n, nil
}

// MissingBlobs queries the CAS to determine if it has the listed blobs. It returns a list of the
// missing blobs.
func (c *Client) MissingBlobs(ctx context.Context, ds []*repb.Digest) ([]*repb.Digest, error) {
	if c.casConcurrency <= 0 {
		return nil, fmt.Errorf("CASConcurrency should be at least 1")
	}
	var batches [][]*repb.Digest
	var missing []*repb.Digest
	var resultMutex sync.Mutex
	const (
		logInterval   = 25
		maxQueryLimit = 10000
	)
	for len(ds) > 0 {
		batchSize := maxQueryLimit
		if len(ds) < maxQueryLimit {
			batchSize = len(ds)
		}
		batch := ds[0:batchSize]
		ds = ds[batchSize:]
		log.V(2).Infof("created query batch of %d blobs", len(batch))
		batches = append(batches, batch)
	}
	log.V(1).Infof("%d query batches created", len(batches))

	eg, eCtx := errgroup.WithContext(ctx)
	todo := make(chan []*repb.Digest, c.casConcurrency)
	for i := 0; i < int(c.casConcurrency) && i < len(batches); i++ {
		eg.Go(func() error {
			for batch := range todo {
				req := &repb.FindMissingBlobsRequest{
					InstanceName: c.InstanceName,
					BlobDigests:  batch,
				}
				resp, err := c.FindMissingBlobs(eCtx, req)
				if err != nil {
					return err
				}
				resultMutex.Lock()
				missing = append(missing, resp.MissingBlobDigests...)
				resultMutex.Unlock()
				if eCtx.Err() != nil {
					return eCtx.Err()
				}
			}
			return nil
		})
	}

	for len(batches) > 0 {
		select {
		case todo <- batches[0]:
			batches = batches[1:]
			if len(batches)%logInterval == 0 {
				log.V(1).Infof("%d missing batches left to query", len(batches))
			}
		case <-eCtx.Done():
			close(todo)
			return nil, eCtx.Err()
		}
	}
	close(todo)
	log.V(1).Info("Waiting for remaining query jobs")
	err := eg.Wait()
	log.V(1).Info("Done")
	return missing, err
}

func (c *Client) resourceNameRead(hash string, sizeBytes int64) string {
	return fmt.Sprintf("%s/blobs/%s/%d", c.InstanceName, hash, sizeBytes)
}

// ResourceNameWrite generates a valid write resource name.
func (c *Client) ResourceNameWrite(hash string, sizeBytes int64) string {
	return fmt.Sprintf("%s/uploads/%s/blobs/%s/%d", c.InstanceName, uuid.New(), hash, sizeBytes)
}

// GetDirectoryTree returns the entire directory tree rooted at the given digest (which must target
// a Directory stored in the CAS).
func (c *Client) GetDirectoryTree(ctx context.Context, d *repb.Digest) (result []*repb.Directory, err error) {
	pageTok := ""
	result = []*repb.Directory{}
	closure := func() error {
		// Use the low-level GetTree method to avoid retrying twice.
		stream, err := c.cas.GetTree(ctx, &repb.GetTreeRequest{
			InstanceName: c.InstanceName,
			RootDigest:   d,
			PageToken:    pageTok,
		})
		if err != nil {
			return err
		}

		for {
			resp, err := stream.Recv()
			if err == io.EOF {
				break
			}
			if err != nil {
				return err
			}
			pageTok = resp.NextPageToken
			result = append(result, resp.Directories...)
		}
		return nil
	}
	if err := c.retrier.do(ctx, closure); err != nil {
		return nil, err
	}
	return result, nil
}

// FlattenActionOutputs collects and flattens all the outputs of an action.
// It downloads the output directory metadata, if required, but not the leaf file blobs.
func (c *Client) FlattenActionOutputs(ctx context.Context, ar *repb.ActionResult) (map[string]*Output, error) {
	outs := make(map[string]*Output)
	for _, file := range ar.OutputFiles {
		outs[file.Path] = &Output{
			Path:         file.Path,
			Digest:       digest.ToKey(file.Digest),
			IsExecutable: file.IsExecutable,
		}
	}
	for _, sm := range ar.OutputFileSymlinks {
		outs[sm.Path] = &Output{
			Path:          sm.Path,
			SymlinkTarget: sm.Target,
		}
	}
	for _, sm := range ar.OutputDirectorySymlinks {
		outs[sm.Path] = &Output{
			Path:          sm.Path,
			SymlinkTarget: sm.Target,
		}
	}
	for _, dir := range ar.OutputDirectories {
		if blob, err := c.ReadBlob(ctx, dir.TreeDigest); err == nil {
			tree := &repb.Tree{}
			if err := proto.Unmarshal(blob, tree); err != nil {
				return nil, err
			}
			dirouts, err := FlattenTree(tree, dir.Path)
			if err != nil {
				return nil, err
			}
			for _, out := range dirouts {
				outs[out.Path] = out
			}
		}
	}
	return outs, nil
}
