package runn

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/bufbuild/protocompile"
	"github.com/bufbuild/protocompile/linker"
	"github.com/goccy/go-json"
	"github.com/jhump/protoreflect/v2/grpcreflect"
	"github.com/k1LoW/runn/version"
	"github.com/mitchellh/copystructure"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/dynamicpb"
)

type GRPCType string

const (
	GRPCUnary           GRPCType = "unary"
	GRPCServerStreaming GRPCType = "server"
	GRPCClientStreaming GRPCType = "client"
	GRPCBidiStreaming   GRPCType = "bidi"
)

type GRPCOp string

const (
	GRPCOpMessage GRPCOp = "message"
	GRPCOpReceive GRPCOp = "receive"
	GRPCOpClose   GRPCOp = "close"
)

const (
	grpcStoreStatusKey   = "status"
	grpcStoreHeaderKey   = "headers"
	grpcStoreTrailerKey  = "trailers"
	grpcStoreMessageKey  = "message"
	grpcStoreMessagesKey = "messages"
	grpcStoreResponseKey = "res"
)

type grpcRunner struct {
	name        string
	target      string
	tls         *bool
	cacert      []byte
	cert        []byte
	key         []byte
	skipVerify  bool
	importPaths []string
	protos      []string
	cc          *grpc.ClientConn
	refc        *grpcreflect.Client
	mds         map[string]protoreflect.MethodDescriptor
	operator    *operator
}

type grpcMessage struct {
	op     GRPCOp
	params map[string]any
}

type grpcRequest struct {
	service  string
	method   string
	headers  metadata.MD
	messages []*grpcMessage
	timeout  time.Duration
}

func newGrpcRunner(name, target string) (*grpcRunner, error) {
	return &grpcRunner{
		name:   name,
		target: target,
		mds:    map[string]protoreflect.MethodDescriptor{},
	}, nil
}

func (rnr *grpcRunner) Close() error {
	if rnr.cc == nil {
		rnr.refc = nil
		return nil
	}
	rnr.refc = nil
	return rnr.cc.Close()
}

func (rnr *grpcRunner) Run(ctx context.Context, r *grpcRequest) error {
	if rnr.cc == nil {
		opts := []grpc.DialOption{
			grpc.WithReturnConnectionError(),
			grpc.WithUserAgent(fmt.Sprintf("runn/%s", version.Version)),
		}
		useTLS := true
		if strings.HasSuffix(rnr.target, ":80") {
			useTLS = false
		}
		if rnr.tls != nil {
			useTLS = *rnr.tls
		}
		if useTLS {
			tlsc := tls.Config{MinVersion: tls.VersionTLS12}
			if len(rnr.cert) != 0 {
				certificate, err := tls.X509KeyPair(rnr.cert, rnr.key)
				if err != nil {
					return err
				}
				tlsc.Certificates = []tls.Certificate{certificate}
			}
			if rnr.skipVerify {
				//#nosec G402
				tlsc.InsecureSkipVerify = true
			} else if len(rnr.cacert) != 0 {
				certpool, err := x509.SystemCertPool()
				if err != nil {
					// FIXME for Windows
					// ref: https://github.com/golang/go/issues/18609
					certpool = x509.NewCertPool()
				}
				if ok := certpool.AppendCertsFromPEM(rnr.cacert); !ok {
					return errors.New("failed to append cacert")
				}
				tlsc.RootCAs = certpool
			}
			opts = append(opts, grpc.WithTransportCredentials(credentials.NewTLS(&tlsc)))
		} else {
			opts = append(opts, grpc.WithTransportCredentials(insecure.NewCredentials()))
		}
		cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		cc, err := grpc.DialContext(cctx, rnr.target, opts...)
		if err != nil {
			return err
		}
		rnr.cc = cc
	}
	if rnr.refc == nil {
		rnr.refc = grpcreflect.NewClientAuto(ctx, rnr.cc)
	}
	if len(rnr.importPaths) > 0 || len(rnr.protos) > 0 {
		if err := rnr.resolveAllMethodsUsingProtos(ctx); err != nil {
			return err
		}
	}
	if len(rnr.mds) == 0 {
		if err := rnr.resolveAllMethodsUsingReflection(ctx); err != nil {
			return err
		}
	}
	key := strings.Join([]string{r.service, r.method}, "/")
	md, ok := rnr.mds[key]
	if !ok {
		return fmt.Errorf("cannot find method: %s", key)
	}
	switch {
	case !md.IsStreamingServer() && !md.IsStreamingClient():
		rnr.operator.capturers.captureGRPCStart(rnr.name, GRPCUnary, r.service, r.method)
		defer rnr.operator.capturers.captureGRPCEnd(rnr.name, GRPCUnary, r.service, r.method)
		return rnr.invokeUnary(ctx, md, r)
	case md.IsStreamingServer() && !md.IsStreamingClient():
		rnr.operator.capturers.captureGRPCStart(rnr.name, GRPCServerStreaming, r.service, r.method)
		defer rnr.operator.capturers.captureGRPCEnd(rnr.name, GRPCServerStreaming, r.service, r.method)
		return rnr.invokeServerStreaming(ctx, md, r)
	case !md.IsStreamingServer() && md.IsStreamingClient():
		rnr.operator.capturers.captureGRPCStart(rnr.name, GRPCClientStreaming, r.service, r.method)
		defer rnr.operator.capturers.captureGRPCEnd(rnr.name, GRPCClientStreaming, r.service, r.method)
		return rnr.invokeClientStreaming(ctx, md, r)
	case md.IsStreamingServer() && md.IsStreamingClient():
		rnr.operator.capturers.captureGRPCStart(rnr.name, GRPCBidiStreaming, r.service, r.method)
		defer rnr.operator.capturers.captureGRPCEnd(rnr.name, GRPCBidiStreaming, r.service, r.method)
		return rnr.invokeBidiStreaming(ctx, md, r)
	default:
		return errors.New("something strange happened")
	}
}

func (rnr *grpcRunner) invokeUnary(ctx context.Context, md protoreflect.MethodDescriptor, r *grpcRequest) error {
	if len(r.messages) != 1 {
		return errors.New("unary RPC message should be 1")
	}
	if r.timeout > 0 {
		cctx, cancel := context.WithTimeout(ctx, r.timeout)
		ctx = cctx
		defer cancel()
	}

	ctx = setHeaders(ctx, r.headers)
	req := dynamicpb.NewMessage(md.Input())

	rnr.operator.capturers.captureGRPCRequestHeaders(r.headers)

	if err := rnr.setMessage(req, r.messages[0].params); err != nil {
		return err
	}

	var (
		resHeaders  metadata.MD
		resTrailers metadata.MD
	)
	res := dynamicpb.NewMessage(md.Output())
	err := rnr.cc.Invoke(ctx, toEndpoint(md.FullName()), req, res, grpc.Header(&resHeaders), grpc.Trailer(&resTrailers))
	stat, ok := status.FromError(err)
	if !ok {
		return err
	}

	d := map[string]any{
		string(grpcStoreStatusKey):  int(stat.Code()),
		string(grpcStoreHeaderKey):  resHeaders,
		string(grpcStoreTrailerKey): resTrailers,
		string(grpcStoreMessageKey): nil,
	}

	rnr.operator.capturers.captureGRPCResponseStatus(stat)
	rnr.operator.capturers.captureGRPCResponseHeaders(resHeaders)
	rnr.operator.capturers.captureGRPCResponseTrailers(resTrailers)

	var messages []map[string]any
	if stat.Code() == codes.OK {
		b, err := protojson.MarshalOptions{UseProtoNames: true, UseEnumNumbers: true, EmitUnpopulated: true}.Marshal(res)
		if err != nil {
			return err
		}
		var msg map[string]any
		if err := json.Unmarshal(b, &msg); err != nil {
			return err
		}
		d[grpcStoreMessageKey] = msg

		rnr.operator.capturers.captureGRPCResponseMessage(msg)

		messages = append(messages, msg)
		d[grpcStoreMessagesKey] = messages
	} else {
		d[grpcStoreMessageKey] = stat.Message()
	}

	rnr.operator.record(map[string]any{
		string(grpcStoreResponseKey): d,
	})
	return nil
}

func (rnr *grpcRunner) invokeServerStreaming(ctx context.Context, md protoreflect.MethodDescriptor, r *grpcRequest) error {
	if len(r.messages) != 1 {
		return errors.New("server streaming RPC message should be 1")
	}
	if r.timeout > 0 {
		cctx, cancel := context.WithTimeout(ctx, r.timeout)
		ctx = cctx
		defer cancel()
	}

	ctx = setHeaders(ctx, r.headers)
	req := dynamicpb.NewMessage(md.Input())

	rnr.operator.capturers.captureGRPCRequestHeaders(r.headers)

	if err := rnr.setMessage(req, r.messages[0].params); err != nil {
		return err
	}

	streamDesc := &grpc.StreamDesc{
		StreamName:    string(md.Name()),
		ServerStreams: md.IsStreamingServer(),
		ClientStreams: md.IsStreamingClient(),
	}

	stream, err := rnr.cc.NewStream(ctx, streamDesc, toEndpoint(md.FullName()))
	if err != nil {
		return err
	}
	if err := stream.SendMsg(req); err != nil {
		return err
	}

	d := map[string]any{
		string(grpcStoreHeaderKey):  metadata.MD{},
		string(grpcStoreTrailerKey): metadata.MD{},
		string(grpcStoreMessageKey): nil,
	}
	var messages []map[string]any

	for err == nil {
		res := dynamicpb.NewMessage(md.Output())
		err = stream.RecvMsg(res)
		if err != nil {
			if errors.Is(err, context.Canceled) {
				break
			}
			if errors.Is(err, io.EOF) {
				break
			}
		}
		stat, ok := status.FromError(err)
		if !ok {
			return err
		}
		d[grpcStoreStatusKey] = int64(stat.Code())

		rnr.operator.capturers.captureGRPCResponseStatus(stat)

		if stat.Code() == codes.OK {
			b, err := protojson.MarshalOptions{UseProtoNames: true, UseEnumNumbers: true, EmitUnpopulated: true}.Marshal(res)
			if err != nil {
				return err
			}
			var msg map[string]any
			if err := json.Unmarshal(b, &msg); err != nil {
				return err
			}
			d[grpcStoreMessageKey] = msg

			rnr.operator.capturers.captureGRPCResponseMessage(msg)

			messages = append(messages, msg)
		} else {
			d[grpcStoreMessageKey] = stat.Message()
		}
	}
	d[grpcStoreMessagesKey] = messages
	if h, err := stream.Header(); err == nil {
		d[grpcStoreHeaderKey] = h

		rnr.operator.capturers.captureGRPCResponseHeaders(h)
	}
	t := stream.Trailer()
	d[grpcStoreTrailerKey] = t

	rnr.operator.capturers.captureGRPCResponseTrailers(t)

	rnr.operator.record(map[string]any{
		string(grpcStoreResponseKey): d,
	})

	return nil
}

func (rnr *grpcRunner) invokeClientStreaming(ctx context.Context, md protoreflect.MethodDescriptor, r *grpcRequest) error {
	if r.timeout > 0 {
		cctx, cancel := context.WithTimeout(ctx, r.timeout)
		ctx = cctx
		defer cancel()
	}

	ctx = setHeaders(ctx, r.headers)

	rnr.operator.capturers.captureGRPCRequestHeaders(r.headers)

	streamDesc := &grpc.StreamDesc{
		StreamName:    string(md.Name()),
		ServerStreams: md.IsStreamingServer(),
		ClientStreams: md.IsStreamingClient(),
	}
	stream, err := rnr.cc.NewStream(ctx, streamDesc, toEndpoint(md.FullName()))
	if err != nil {
		return err
	}
	d := map[string]any{
		string(grpcStoreHeaderKey):  metadata.MD{},
		string(grpcStoreTrailerKey): metadata.MD{},
		string(grpcStoreMessageKey): nil,
	}
	var messages []map[string]any
	for _, m := range r.messages {
		switch m.op {
		case GRPCOpMessage:
			req := dynamicpb.NewMessage(md.Input())

			if err := rnr.setMessage(req, m.params); err != nil {
				return err
			}

			err := stream.SendMsg(req)
			if errors.Is(err, context.Canceled) {
				break
			}
			if errors.Is(err, io.EOF) {
				break
			}
		default:
			return fmt.Errorf("invalid op: %v", m.op)
		}
	}
	res := dynamicpb.NewMessage(md.Output())
	if err := stream.CloseSend(); err != nil {
		return err
	}
	err = stream.RecvMsg(res)
	stat, ok := status.FromError(err)
	if !ok {
		return err
	}

	d[grpcStoreStatusKey] = int64(stat.Code())

	rnr.operator.capturers.captureGRPCResponseStatus(stat)

	if stat.Code() == codes.OK {
		b, err := protojson.MarshalOptions{UseProtoNames: true, UseEnumNumbers: true, EmitUnpopulated: true}.Marshal(res)
		if err != nil {
			return err
		}
		var msg map[string]any
		if err := json.Unmarshal(b, &msg); err != nil {
			return err
		}
		d[grpcStoreMessageKey] = msg

		rnr.operator.capturers.captureGRPCResponseMessage(msg)

		messages = append(messages, msg)
	} else {
		d[grpcStoreMessageKey] = stat.Message()
	}

	d[grpcStoreMessagesKey] = messages
	if h, err := stream.Header(); err == nil {
		d[grpcStoreHeaderKey] = h

		rnr.operator.capturers.captureGRPCResponseHeaders(h)
	}
	t := stream.Trailer()
	d[grpcStoreTrailerKey] = t

	rnr.operator.capturers.captureGRPCResponseTrailers(t)

	rnr.operator.record(map[string]any{
		string(grpcStoreResponseKey): d,
	})

	return nil
}

func (rnr *grpcRunner) invokeBidiStreaming(ctx context.Context, md protoreflect.MethodDescriptor, r *grpcRequest) error {
	if r.timeout > 0 {
		return errors.New("unsupported timeout: for bidirectional streaming RPC")
	}

	ctx = setHeaders(ctx, r.headers)
	rnr.operator.capturers.captureGRPCRequestHeaders(r.headers)

	streamDesc := &grpc.StreamDesc{
		StreamName:    string(md.Name()),
		ServerStreams: md.IsStreamingServer(),
		ClientStreams: md.IsStreamingClient(),
	}

	stream, err := rnr.cc.NewStream(ctx, streamDesc, toEndpoint(md.FullName()))
	if err != nil {
		return err
	}

	d := map[string]any{
		string(grpcStoreHeaderKey):  metadata.MD{},
		string(grpcStoreTrailerKey): metadata.MD{},
		string(grpcStoreMessageKey): nil,
	}
	var messages []map[string]any
	clientClose := false
L:
	for _, m := range r.messages {
		switch m.op {
		case GRPCOpMessage:
			req := dynamicpb.NewMessage(md.Input())
			if err := rnr.setMessage(req, m.params); err != nil {
				return err
			}
			err = stream.SendMsg(req)
			if errors.Is(err, context.Canceled) {
				break L
			}
			if errors.Is(err, io.EOF) {
				break L
			}

			req.Reset()
		case GRPCOpReceive:
			res := dynamicpb.NewMessage(md.Output())
			err := stream.RecvMsg(res)
			if errors.Is(err, context.Canceled) {
				break L
			}
			if errors.Is(err, io.EOF) {
				break L
			}
			stat, ok := status.FromError(err)
			if !ok {
				return err
			}
			d[grpcStoreStatusKey] = int64(stat.Code())

			rnr.operator.capturers.captureGRPCResponseStatus(stat)

			if h, err := stream.Header(); err == nil {
				d[grpcStoreHeaderKey] = h

				rnr.operator.capturers.captureGRPCResponseHeaders(h)
			}
			if stat.Code() == codes.OK {
				b, err := protojson.MarshalOptions{UseProtoNames: true, UseEnumNumbers: true, EmitUnpopulated: true}.Marshal(res)
				if err != nil {
					return err
				}
				var msg map[string]any
				if err := json.Unmarshal(b, &msg); err != nil {
					return err
				}
				d[grpcStoreMessageKey] = msg

				rnr.operator.capturers.captureGRPCResponseMessage(msg)

				messages = append(messages, msg)
			} else {
				d[grpcStoreMessageKey] = stat.Message()
			}
		case GRPCOpClose:
			clientClose = true
			err = stream.CloseSend()
			rnr.operator.capturers.captureGRPCClientClose()
			break L
		default:
			return fmt.Errorf("invalid op: %v", m.op)
		}
	}
	stat, ok := status.FromError(err)
	if !ok {
		return err
	}
	if stat.Code() != codes.OK {
		d[grpcStoreStatusKey] = int64(stat.Code())
		d[grpcStoreMessageKey] = stat.Message()

		rnr.operator.capturers.captureGRPCResponseStatus(stat)
	}

	if clientClose {
		for {
			res := dynamicpb.NewMessage(md.Output())
			if err := stream.RecvMsg(res); err != nil {
				if errors.Is(err, context.Canceled) {
					break
				}
				if errors.Is(err, io.EOF) {
					break
				}
				return err
			} else {
				if err := stream.CloseSend(); err != nil {
					return err
				}
			}
		}
	} else {
		if err == nil {
			for {
				res := dynamicpb.NewMessage(md.Output())
				err := stream.RecvMsg(res)
				if errors.Is(err, context.Canceled) {
					break
				}
				if errors.Is(err, io.EOF) {
					break
				}
				stat, ok := status.FromError(err)
				if !ok {
					return err
				}
				d[grpcStoreStatusKey] = int64(stat.Code())

				rnr.operator.capturers.captureGRPCResponseStatus(stat)
				if stat.Code() == codes.OK {
					b, err := protojson.MarshalOptions{UseProtoNames: true, UseEnumNumbers: true, EmitUnpopulated: true}.Marshal(res)
					if err != nil {
						return err
					}
					var msg map[string]any
					if err := json.Unmarshal(b, &msg); err != nil {
						return err
					}
					d[grpcStoreMessageKey] = msg

					rnr.operator.capturers.captureGRPCResponseMessage(msg)

					messages = append(messages, msg)
				} else {
					d[grpcStoreMessageKey] = stat.Message()
				}
			}
		}
	}

	// If the connection is not disconnected here, it will fall into a race condition when retrieving the trailer.
	if err := rnr.cc.Close(); err != nil {
		return err
	}
	rnr.cc = nil
	rnr.refc = nil

	d[grpcStoreMessagesKey] = messages
	if h, err := stream.Header(); len(d[grpcStoreHeaderKey].(metadata.MD)) == 0 && err == nil {
		d[grpcStoreHeaderKey] = h
	}
	t, ok := dcopy(stream.Trailer()).(metadata.MD)
	if !ok {
		return fmt.Errorf("failed to copy trailers: %s", t)
	}
	d[grpcStoreTrailerKey] = t

	rnr.operator.capturers.captureGRPCResponseTrailers(t)

	rnr.operator.record(map[string]any{
		string(grpcStoreResponseKey): d,
	})

	return nil
}

func setHeaders(ctx context.Context, h metadata.MD) context.Context {
	var kv []string
	for k, v := range h {
		kv = append(kv, k)
		kv = append(kv, v...)
	}
	ctx = metadata.AppendToOutgoingContext(ctx, kv...)
	return ctx
}

func (rnr *grpcRunner) setMessage(req proto.Message, message map[string]any) error {
	// Lazy expand due to the possibility of computing variables between multiple messages.
	e, err := rnr.operator.expandBeforeRecord(message)
	if err != nil {
		return err
	}
	m, ok := e.(map[string]any)
	if !ok {
		return fmt.Errorf("invalid message: %v", e)
	}
	rnr.operator.capturers.captureGRPCRequestMessage(m)
	b, err := json.Marshal(e)
	if err != nil {
		return err
	}
	return protojson.Unmarshal(b, req)
}

func (rnr *grpcRunner) resolveAllMethodsUsingReflection(ctx context.Context) error {
	svcs, err := rnr.refc.ListServices()
	if err != nil {
		return err
	}
	for _, svc := range svcs {
		d, err := rnr.findDescripter(svc)
		if err != nil {
			return fmt.Errorf("failed to find descriptor: %w", err)
		}
		sd, ok := d.(protoreflect.ServiceDescriptor)
		if !ok {
			return fmt.Errorf("invalid descriptor: %v", d)
		}
		mds := sd.Methods()
		for j := 0; j < mds.Len(); j++ {
			md := mds.Get(j)
			key := strings.Join([]string{string(sd.FullName()), string(md.Name())}, "/")
			rnr.mds[key] = md
		}
	}
	return nil
}

func (rnr *grpcRunner) findDescripter(svc protoreflect.FullName) (protoreflect.Descriptor, error) {
	d, err := protoregistry.GlobalFiles.FindDescriptorByName(svc)
	if err != nil && !errors.Is(err, protoregistry.NotFound) {
		return nil, err
	}
	if err == nil {
		return d, nil
	}
	fd, err := rnr.refc.FileContainingSymbol(svc)
	if err != nil {
		return nil, err
	}
	if err := protoregistry.GlobalFiles.RegisterFile(fd); err != nil {
		return nil, err
	}
	return protoregistry.GlobalFiles.FindDescriptorByName(svc)
}

func (rnr *grpcRunner) resolveAllMethodsUsingProtos(ctx context.Context) error {
	protos, err := fetchPaths(strings.Join(rnr.protos, string(os.PathListSeparator)))
	if err != nil {
		return err
	}
	importPaths, protos, err := resolvePaths(rnr.importPaths, protos...)
	if err != nil {
		return err
	}
	comp := protocompile.Compiler{
		Resolver: protocompile.WithStandardImports(&protocompile.SourceResolver{
			ImportPaths: importPaths,
		}),
	}
	fds, err := comp.Compile(ctx, protos...)
	if err != nil {
		return err
	}
	if err := registerFiles(fds); err != nil {
		return err
	}
	for _, fd := range fds {
		for i := 0; i < fd.Services().Len(); i++ {
			svc := fd.Services().Get(i)
			for j := 0; j < svc.Methods().Len(); j++ {
				m := svc.Methods().Get(j)
				key := fmt.Sprintf("%s/%s", svc.FullName(), m.Name())
				rnr.mds[key] = m
			}
		}
	}
	return nil
}

func dcopy(in any) any {
	return copystructure.Must(copystructure.Copy(in))
}

func toEndpoint(mn protoreflect.FullName) string {
	splitted := strings.Split(string(mn), ".")
	service := strings.Join(splitted[:len(splitted)-1], ".")
	method := splitted[len(splitted)-1]
	return fmt.Sprintf("/%s/%s", service, method)
}

func registerFiles(fds linker.Files) (err error) {
	for _, fd := range fds {
		// Skip registration of already registered descriptors
		if _, err := protoregistry.GlobalFiles.FindFileByPath(fd.Path()); !errors.Is(protoregistry.NotFound, err) {
			continue
		}
		// Skip registration of conflicted descriptors
		conflict := false
		rangeTopLevelDescriptors(fd, func(d protoreflect.Descriptor) {
			if _, err := protoregistry.GlobalFiles.FindDescriptorByName(d.FullName()); err == nil {
				conflict = true
			}
		})
		if conflict {
			continue
		}

		if err := protoregistry.GlobalFiles.RegisterFile(fd); err != nil {
			return err
		}
	}
	return nil
}

// copy from google.golang.org/protobuf/reflect/protoregistry.
func rangeTopLevelDescriptors(fd protoreflect.FileDescriptor, f func(protoreflect.Descriptor)) {
	eds := fd.Enums()
	for i := eds.Len() - 1; i >= 0; i-- {
		f(eds.Get(i))
		vds := eds.Get(i).Values()
		for i := vds.Len() - 1; i >= 0; i-- {
			f(vds.Get(i))
		}
	}
	mds := fd.Messages()
	for i := mds.Len() - 1; i >= 0; i-- {
		f(mds.Get(i))
	}
	xds := fd.Extensions()
	for i := xds.Len() - 1; i >= 0; i-- {
		f(xds.Get(i))
	}
	sds := fd.Services()
	for i := sds.Len() - 1; i >= 0; i-- {
		f(sds.Get(i))
	}
}

func resolvePaths(importPaths []string, protos ...string) ([]string, []string, error) {
	const sep = string(filepath.Separator)
	if len(importPaths) == 0 {
		return importPaths, protos, nil
	}
	importPaths = unique(importPaths)
	var resolvedIPaths []string
	for _, p := range importPaths {
		abs, err := filepath.Abs(p)
		if err != nil {
			return nil, nil, err
		}
		resolvedIPaths = append(resolvedIPaths, abs)
	}
	resolvedIPaths = unique(resolvedIPaths)
	var resolvedProtos []string
	for _, p := range protos {
		abs, err := filepath.Abs(p)
		if err != nil {
			return nil, nil, err
		}
		for _, ip := range resolvedIPaths {
			if strings.HasPrefix(abs, ip+sep) {
				resolvedProtos = append(resolvedProtos, strings.TrimPrefix(abs, ip+sep))
			}
		}
	}
	resolvedProtos = unique(resolvedProtos)
	return resolvedIPaths, resolvedProtos, nil
}
