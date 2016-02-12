// Copyright 2015 CoreOS, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package clientv3

import (
	"sync"

	"github.com/coreos/etcd/Godeps/_workspace/src/golang.org/x/net/context"
	"github.com/coreos/etcd/Godeps/_workspace/src/google.golang.org/grpc"
	pb "github.com/coreos/etcd/etcdserver/etcdserverpb"
)

type (
	PutResponse    pb.PutResponse
	GetResponse    pb.RangeResponse
	DeleteResponse pb.DeleteRangeResponse
	TxnResponse    pb.TxnResponse
)

type KV interface {
	// PUT puts a key-value pair into etcd.
	// Note that key,value can be plain bytes array and string is
	// an immutable representation of that bytes array.
	// To get a string of bytes, do string([]byte(0x10, 0x20)).
	Put(ctx context.Context, key, val string, opts ...OpOption) (*PutResponse, error)

	// Get retrieves keys.
	// By default, Get will return the value for "key", if any.
	// When passed WithRange(end), Get will return the keys in the range [key, end) if
	// end is non-empty, otherwise it returns keys greater than or equal to key.
	// When passed WithRev(rev) with rev > 0, Get retrieves keys at the given revision;
	// if the required revision is compacted, the request will fail with ErrCompacted .
	// When passed WithLimit(limit), the number of returned keys is bounded by limit.
	// When passed WithSort(), the keys will be sorted.
	Get(ctx context.Context, key string, opts ...OpOption) (*GetResponse, error)

	// Delete deletes a key, or optionallly using WithRange(end), [key, end).
	Delete(ctx context.Context, key string, opts ...OpOption) (*DeleteResponse, error)

	// Compact compacts etcd KV history before the given rev.
	Compact(ctx context.Context, rev int64) error

	// Txn creates a transaction.
	Txn(ctx context.Context) Txn
}

type kv struct {
	c *Client

	mu     sync.Mutex       // guards all fields
	conn   *grpc.ClientConn // conn in-use
	remote pb.KVClient
}

func NewKV(c *Client) KV {
	conn := c.ActiveConnection()
	remote := pb.NewKVClient(conn)

	return &kv{
		conn:   c.ActiveConnection(),
		remote: remote,

		c: c,
	}
}

func (kv *kv) Put(ctx context.Context, key, val string, opts ...OpOption) (*PutResponse, error) {
	r, err := kv.do(ctx, OpPut(key, val, opts...))
	if err != nil {
		return nil, err
	}
	return (*PutResponse)(r.GetResponsePut()), nil
}

func (kv *kv) Get(ctx context.Context, key string, opts ...OpOption) (*GetResponse, error) {
	r, err := kv.do(ctx, OpGet(key, opts...))
	if err != nil {
		return nil, err
	}
	return (*GetResponse)(r.GetResponseRange()), nil
}

func (kv *kv) Delete(ctx context.Context, key string, opts ...OpOption) (*DeleteResponse, error) {
	r, err := kv.do(ctx, OpDelete(key, opts...))
	if err != nil {
		return nil, err
	}
	return (*DeleteResponse)(r.GetResponseDeleteRange()), nil
}

func (kv *kv) Compact(ctx context.Context, rev int64) error {
	r := &pb.CompactionRequest{Revision: rev}
	_, err := kv.getRemote().Compact(ctx, r)
	if err == nil {
		return nil
	}

	if isRPCError(err) {
		return err
	}

	go kv.switchRemote(err)
	return err
}

func (kv *kv) Txn(ctx context.Context) Txn {
	return &txn{
		kv:  kv,
		ctx: ctx,
	}
}

func (kv *kv) do(ctx context.Context, op Op) (*pb.ResponseUnion, error) {
	for {
		var err error
		switch op.t {
		// TODO: handle other ops
		case tRange:
			var resp *pb.RangeResponse
			r := &pb.RangeRequest{Key: op.key, RangeEnd: op.end, Limit: op.limit, Revision: op.rev}
			if op.sort != nil {
				r.SortOrder = pb.RangeRequest_SortOrder(op.sort.Order)
				r.SortTarget = pb.RangeRequest_SortTarget(op.sort.Target)
			}

			resp, err = kv.getRemote().Range(ctx, r)
			if err == nil {
				respu := &pb.ResponseUnion_ResponseRange{ResponseRange: resp}
				return &pb.ResponseUnion{Response: respu}, nil
			}
		case tPut:
			var resp *pb.PutResponse
			r := &pb.PutRequest{Key: op.key, Value: op.val, Lease: int64(op.leaseID)}
			resp, err = kv.getRemote().Put(ctx, r)
			if err == nil {
				respu := &pb.ResponseUnion_ResponsePut{ResponsePut: resp}
				return &pb.ResponseUnion{Response: respu}, nil
			}
		case tDeleteRange:
			var resp *pb.DeleteRangeResponse
			r := &pb.DeleteRangeRequest{Key: op.key, RangeEnd: op.end}
			resp, err = kv.getRemote().DeleteRange(ctx, r)
			if err == nil {
				respu := &pb.ResponseUnion_ResponseDeleteRange{ResponseDeleteRange: resp}
				return &pb.ResponseUnion{Response: respu}, nil
			}
		default:
			panic("Unknown op")
		}

		if isRPCError(err) {
			return nil, err
		}

		// do not retry on modifications
		if op.isWrite() {
			go kv.switchRemote(err)
			return nil, err
		}

		if nerr := kv.switchRemote(err); nerr != nil {
			return nil, nerr
		}
	}
}

func (kv *kv) switchRemote(prevErr error) error {
	newConn, err := kv.c.retryConnection(kv.conn, prevErr)
	if err != nil {
		return err
	}

	kv.mu.Lock()
	defer kv.mu.Unlock()

	kv.conn = newConn
	kv.remote = pb.NewKVClient(kv.conn)
	return nil
}

func (kv *kv) getRemote() pb.KVClient {
	kv.mu.Lock()
	defer kv.mu.Unlock()
	return kv.remote
}