package client_test

import (
	"bytes"
	"context"
	"io"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/bazelbuild/remote-apis-sdks/go/client"
	"github.com/bazelbuild/remote-apis-sdks/go/digest"
	"github.com/google/go-cmp/cmp"
	"github.com/pborman/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	regrpc "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	repb "github.com/bazelbuild/remote-apis/build/bazel/remote/execution/v2"
	bsgrpc "google.golang.org/genproto/googleapis/bytestream"
	bspb "google.golang.org/genproto/googleapis/bytestream"
)

// fakeReader implements ByteStream's Read interface, returning one blob.
type fakeReader struct {
	// blob is the blob being read.
	blob []byte
	// chunks is a list of chunk sizes, in the order they are produced. The sum must be equal to the
	// length of blob.
	chunks []int
}

// validate ensures that a fakeReader has the chunk sizes set correctly.
func (f *fakeReader) validate(t *testing.T) {
	t.Helper()
	sum := 0
	for _, c := range f.chunks {
		if c < 0 {
			t.Errorf("Invalid chunk specification: chunk with negative size %d", c)
		}
		sum += c
	}
	if sum != len(f.blob) {
		t.Errorf("Invalid chunk specification: chunk sizes sum to %d but blob is length %d", sum, len(f.blob))
	}
}

func (f *fakeReader) Read(req *bspb.ReadRequest, stream bsgrpc.ByteStream_ReadServer) error {
	path := strings.Split(req.ResourceName, "/")
	if len(path) != 4 || path[0] != "instance" || path[1] != "blobs" {
		return status.Error(codes.InvalidArgument, "test fake expected resource name of the form \"instance/blobs/<hash>/<size>\"")
	}
	dg := digest.FromBlob(f.blob)
	if path[2] != dg.Hash || path[3] != strconv.FormatInt(dg.SizeBytes, 10) {
		return status.Errorf(codes.NotFound, "test fake only has blob with digest %s, but %s/%s was requested", digest.ToString(dg), path[2], path[3])
	}

	offset := req.ReadOffset
	limit := req.ReadLimit
	blob := f.blob
	chunks := f.chunks
	for len(chunks) > 0 {
		buf := blob[:chunks[0]]
		if offset >= int64(len(buf)) {
			offset -= int64(len(buf))
		} else {
			if offset > 0 {
				buf = buf[offset:]
				offset = 0
			}
			if limit > 0 {
				if limit < int64(len(buf)) {
					buf = buf[:limit]
				}
				limit -= int64(len(buf))
			}
			if err := stream.Send(&bspb.ReadResponse{Data: buf}); err != nil {
				return err
			}
			if limit == 0 && req.ReadLimit != 0 {
				break
			}
		}
		blob = blob[chunks[0]:]
		chunks = chunks[1:]
	}
	return nil
}

func (f *fakeReader) Write(bsgrpc.ByteStream_WriteServer) error {
	return status.Error(codes.Unimplemented, "test fake does not implement method")
}

func (f *fakeReader) QueryWriteStatus(context.Context, *bspb.QueryWriteStatusRequest) (*bspb.QueryWriteStatusResponse, error) {
	return nil, status.Error(codes.Unimplemented, "test fake does not implement method")
}

// fakeWriter expects to receive Write calls and fills the buffer.
type fakeWriter struct {
	// buf is a buffer that is set to the contents of a Write call after one is received.
	buf []byte
	// err is a copy of the error returned by Write.
	err error
}

func (f *fakeWriter) Write(stream bsgrpc.ByteStream_WriteServer) (err error) {
	// Store the error so we can verify that the client didn't drop the stream early, meaning the
	// request won't error.
	defer func() { f.err = err }()

	off := int64(0)
	buf := new(bytes.Buffer)

	req, err := stream.Recv()
	if err == io.EOF {
		return status.Error(codes.InvalidArgument, "no write request received")
	}
	if err != nil {
		return err
	}

	path := strings.Split(req.ResourceName, "/")
	if len(path) != 6 || path[0] != "instance" || path[1] != "uploads" || path[3] != "blobs" {
		return status.Error(codes.InvalidArgument, "test fake expected resource name of the form \"instance/uploads/<uuid>/blobs/<hash>/<size>\"")
	}
	size, err := strconv.ParseInt(path[5], 10, 64)
	if err != nil {
		return status.Error(codes.InvalidArgument, "test fake expected resource name of the form \"instance/uploads/<uuid>/blobs/<hash>/<size>\"")
	}
	dg := &repb.Digest{Hash: path[4], SizeBytes: size}
	if uuid.Parse(path[2]) == nil {
		return status.Error(codes.InvalidArgument, "test fake expected resource name of the form \"instance/uploads/<uuid>/blobs/<hash>/<size>\"")
	}

	res := req.ResourceName
	done := false
	for {
		if req.ResourceName != res && req.ResourceName != "" {
			return status.Errorf(codes.InvalidArgument, "follow-up request had resource name %q different from original %q", req.ResourceName, res)
		}
		if req.WriteOffset != off {
			return status.Errorf(codes.InvalidArgument, "request had incorrect offset %d, expected %d", req.WriteOffset, off)
		}
		if done {
			return status.Errorf(codes.InvalidArgument, "received write request after the client finished writing")
		}
		// 2 MB is the protocol max.
		if len(req.Data) > 2*1024*1024 {
			return status.Errorf(codes.InvalidArgument, "data chunk greater than 2MB")
		}

		// bytes.Buffer.Write can't error
		_, _ = buf.Write(req.Data)
		off += int64(len(req.Data))
		if req.FinishWrite {
			done = true
		}

		req, err = stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
	}

	if !done {
		return status.Errorf(codes.InvalidArgument, "reached end of stream before the client finished writing")
	}

	f.buf = buf.Bytes()
	recvDg := digest.FromBlob(f.buf)
	if diff := cmp.Diff(dg, recvDg); diff != "" {
		return status.Errorf(codes.InvalidArgument, "mismatched digest with diff:\n%s", diff)
	}
	return stream.SendAndClose(&bspb.WriteResponse{CommittedSize: dg.SizeBytes})
}

func (f *fakeWriter) Read(*bspb.ReadRequest, bsgrpc.ByteStream_ReadServer) error {
	return status.Error(codes.Unimplemented, "test fake does not implement method")
}

func (f *fakeWriter) QueryWriteStatus(context.Context, *bspb.QueryWriteStatusRequest) (*bspb.QueryWriteStatusResponse, error) {
	return nil, status.Error(codes.Unimplemented, "test fake does not implement method")
}

// fakeMultiCAS is a fake CAS that implements FindMissingBlobs, Read and Write, storing stored blobs
// in a map. It also counts the number of requests to store received, for validating batching logic.
type fakeCAS struct {
	// blobs is the list of blobs that are considered present in the CAS.
	blobs     map[digest.Key][]byte
	mu        sync.RWMutex
	batchReqs int
	writeReqs int
}

func (f *fakeCAS) FindMissingBlobs(ctx context.Context, req *repb.FindMissingBlobsRequest) (*repb.FindMissingBlobsResponse, error) {
	f.mu.RLock()
	defer f.mu.RUnlock()

	if req.InstanceName != "instance" {
		return nil, status.Error(codes.InvalidArgument, "test fake expected instance name \"instance\"")
	}
	resp := new(repb.FindMissingBlobsResponse)
	for _, dg := range req.BlobDigests {
		if _, ok := f.blobs[digest.ToKey(dg)]; !ok {
			resp.MissingBlobDigests = append(resp.MissingBlobDigests, dg)
		}
	}
	return resp, nil
}

func (f *fakeCAS) BatchUpdateBlobs(ctx context.Context, req *repb.BatchUpdateBlobsRequest) (*repb.BatchUpdateBlobsResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.batchReqs++

	if req.InstanceName != "instance" {
		return nil, status.Error(codes.InvalidArgument, "test fake expected instance name \"instance\"")
	}

	var tot int64
	for _, r := range req.Requests {
		tot += r.Digest.SizeBytes
	}
	if tot > client.MaxBatchSz {
		return nil, status.Errorf(codes.InvalidArgument, "test fake received batch update for more than the maximum of %d bytes: %d bytes", client.MaxBatchSz, tot)
	}

	var resps []*repb.BatchUpdateBlobsResponse_Response
	for _, r := range req.Requests {
		dg := digest.FromBlob(r.Data)
		key := digest.ToKey(dg)
		if key != digest.ToKey(r.Digest) {
			resps = append(resps, &repb.BatchUpdateBlobsResponse_Response{
				Digest: r.Digest,
				Status: status.Newf(codes.InvalidArgument, "Digest mismatch: digest of data was %s but digest of content was %s",
					digest.ToString(dg), digest.ToString(r.Digest)).Proto(),
			})
			continue
		}
		f.blobs[key] = r.Data
		resps = append(resps, &repb.BatchUpdateBlobsResponse_Response{
			Digest: r.Digest,
			Status: status.New(codes.OK, "").Proto(),
		})
	}
	return &repb.BatchUpdateBlobsResponse{Responses: resps}, nil
}

func (f *fakeCAS) BatchReadBlobs(ctx context.Context, req *repb.BatchReadBlobsRequest) (*repb.BatchReadBlobsResponse, error) {
	return nil, status.Error(codes.Unimplemented, "test fake does not implement method")
}

func (f *fakeCAS) GetTree(*repb.GetTreeRequest, regrpc.ContentAddressableStorage_GetTreeServer) error {
	return status.Error(codes.Unimplemented, "test fake does not implement method")
}

func (f *fakeCAS) Write(stream bsgrpc.ByteStream_WriteServer) (err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.writeReqs++

	off := int64(0)
	buf := new(bytes.Buffer)

	req, err := stream.Recv()
	if err == io.EOF {
		return status.Error(codes.InvalidArgument, "no write request received")
	}
	if err != nil {
		return err
	}

	path := strings.Split(req.ResourceName, "/")
	if len(path) != 6 || path[0] != "instance" || path[1] != "uploads" || path[3] != "blobs" {
		return status.Error(codes.InvalidArgument, "test fake expected resource name of the form \"instance/uploads/<uuid>/blobs/<hash>/<size>\"")
	}
	size, err := strconv.ParseInt(path[5], 10, 64)
	if err != nil {
		return status.Error(codes.InvalidArgument, "test fake expected resource name of the form \"instance/uploads/<uuid>/blobs/<hash>/<size>\"")
	}
	dg := &repb.Digest{Hash: path[4], SizeBytes: size}
	if uuid.Parse(path[2]) == nil {
		return status.Error(codes.InvalidArgument, "test fake expected resource name of the form \"instance/uploads/<uuid>/blobs/<hash>/<size>\"")
	}

	res := req.ResourceName
	done := false
	for {
		if req.ResourceName != res && req.ResourceName != "" {
			return status.Errorf(codes.InvalidArgument, "follow-up request had resource name %q different from original %q", req.ResourceName, res)
		}
		if req.WriteOffset != off {
			return status.Errorf(codes.InvalidArgument, "request had incorrect offset %d, expected %d", req.WriteOffset, off)
		}
		if done {
			return status.Errorf(codes.InvalidArgument, "received write request after the client finished writing")
		}
		// 2 MB is the protocol max.
		if len(req.Data) > 2*1024*1024 {
			return status.Errorf(codes.InvalidArgument, "data chunk greater than 2MB")
		}

		// bytes.Buffer.Write can't error
		_, _ = buf.Write(req.Data)
		off += int64(len(req.Data))
		if req.FinishWrite {
			done = true
		}

		req, err = stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
	}

	if !done {
		return status.Errorf(codes.InvalidArgument, "reached end of stream before the client finished writing")
	}

	f.blobs[digest.ToKey(dg)] = buf.Bytes()
	recvDg := digest.FromBlob(f.blobs[digest.ToKey(dg)])
	if diff := cmp.Diff(dg, recvDg); diff != "" {
		delete(f.blobs, digest.ToKey(dg))
		return status.Errorf(codes.InvalidArgument, "mismatched digest with diff:\n%s", diff)
	}
	return stream.SendAndClose(&bspb.WriteResponse{CommittedSize: dg.SizeBytes})
}

func (f *fakeCAS) Read(req *bspb.ReadRequest, stream bsgrpc.ByteStream_ReadServer) error {
	if req.ReadOffset != 0 || req.ReadLimit != 0 {
		return status.Error(codes.Unimplemented, "test fake does not implement read_offset or limit")
	}

	path := strings.Split(req.ResourceName, "/")
	if len(path) != 4 || path[0] != "instance" || path[1] != "blobs" {
		return status.Error(codes.InvalidArgument, "test fake expected resource name of the form \"instance/blobs/<hash>/<size>\"")
	}
	size, err := strconv.Atoi(path[3])
	if err != nil {
		return status.Error(codes.InvalidArgument, "test fake expected resource name of the form \"instance/blobs/<hash>/<size>\"")
	}
	dg := digest.TestNew(path[2], int64(size))
	blob, ok := f.blobs[digest.ToKey(dg)]
	if !ok {
		return status.Errorf(codes.NotFound, "test fake missing blob with digest %s was requested", digest.ToString(dg))
	}

	return stream.Send(&bspb.ReadResponse{Data: blob})
}

func (f *fakeCAS) QueryWriteStatus(context.Context, *bspb.QueryWriteStatusRequest) (*bspb.QueryWriteStatusResponse, error) {
	return nil, status.Error(codes.Unimplemented, "test fake does not implement method")
}
