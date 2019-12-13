// SPDX-License-Identifier: MIT

package sbot

import (
	"context"
	"io"
	"net"
	"sync"
	"time"

	kitlog "github.com/go-kit/kit/log"
	"github.com/go-kit/kit/log/level"
	"github.com/go-kit/kit/metrics"
	"github.com/pkg/errors"
	"go.cryptoscope.co/librarian"
	"go.cryptoscope.co/margaret/multilog"
	"go.cryptoscope.co/muxrpc"
	"golang.org/x/sync/errgroup"

	"go.cryptoscope.co/ssb"
	"go.cryptoscope.co/ssb/blobstore"
	"go.cryptoscope.co/ssb/graph"
	"go.cryptoscope.co/ssb/indexes"
	"go.cryptoscope.co/ssb/internal/ctxutils"
	"go.cryptoscope.co/ssb/internal/mutil"
	"go.cryptoscope.co/ssb/message"
	"go.cryptoscope.co/ssb/message/multimsg"
	"go.cryptoscope.co/ssb/multilogs"
	"go.cryptoscope.co/ssb/network"
	"go.cryptoscope.co/ssb/plugins/blobs"
	"go.cryptoscope.co/ssb/plugins/get"
	"go.cryptoscope.co/ssb/plugins/gossip"
	privplug "go.cryptoscope.co/ssb/plugins/private"
	"go.cryptoscope.co/ssb/plugins/publish"
	"go.cryptoscope.co/ssb/plugins/rawread"
	"go.cryptoscope.co/ssb/plugins/replicate"
	"go.cryptoscope.co/ssb/plugins/status"
	"go.cryptoscope.co/ssb/plugins/whoami"
	"go.cryptoscope.co/ssb/private"
	"go.cryptoscope.co/ssb/repo"
)

type Sbot struct {
	info kitlog.Logger

	// TODO: this thing is way to big right now
	// because it's options and the resulting thing at once

	lateInit []Option

	rootCtx  context.Context
	Shutdown context.CancelFunc
	closers  multiCloser
	idxDone  errgroup.Group

	closed   bool
	closedMu sync.Mutex
	closeErr error

	promisc  bool
	hopCount uint

	appKey []byte

	networks     []ssb.Network
	muxservGroup errgroup.Group

	// TODO: these should all be options that are applied on the network construction...
	// listenAddr         net.Addr
	// dialer             netwrap.Dialer
	// edpWrapper         MuxrpcEndpointWrapper
	// networkConnTracker ssb.ConnTracker
	// preSecureWrappers  []netwrap.ConnWrapper
	// postSecureWrappers []netwrap.ConnWrapper
	// enableAdverts   bool
	// enableDiscovery bool

	public ssb.PluginManager
	master ssb.PluginManager

	repoPath         string
	KeyPair          *ssb.KeyPair
	RootLog          multimsg.AlterableLog
	liveIndexUpdates bool

	PublishLog     ssb.Publisher
	signHMACsecret []byte

	mlogIndicies map[string]multilog.MultiLog
	simpleIndex  map[string]librarian.Index

	GraphBuilder graph.Builder

	BlobStore   ssb.BlobStore
	WantManager ssb.WantManager

	// TODO: wrap better
	eventCounter metrics.Counter
	systemGauge  metrics.Gauge
	latency      metrics.Histogram
}

func (s *Sbot) Close() error {
	s.closedMu.Lock()
	defer s.closedMu.Unlock()

	if s.closed {
		return s.closeErr
	}

	closeEvt := kitlog.With(s.info, "event", "sbot closing")
	s.closed = true

	for i, n := range s.networks {
		if err := n.Close(); err != nil {
			s.closeErr = errors.Wrapf(err, "sbot: failed to close network node #%d", i)
			return s.closeErr
		}
		n.GetConnTracker().CloseAll()
		level.Debug(closeEvt).Log("msg", "connections closed", "i", i)
	}

	if err := s.muxservGroup.Wait(); err != nil {
		s.closeErr = errors.Wrap(err, "sbot: muxrpc serve group shutdown failed")
		return s.closeErr
	}

	if err := s.idxDone.Wait(); err != nil {
		s.closeErr = errors.Wrap(err, "sbot: index group shutdown failed")
		return s.closeErr
	}
	level.Debug(closeEvt).Log("msg", "waited for indexes to close")

	if err := s.closers.Close(); err != nil {
		s.closeErr = err
		return s.closeErr
	}
	level.Info(closeEvt).Log("msg", "closers closed")
	return nil
}

func initSbot(s *Sbot) (*Sbot, error) {
	log := s.info
	var err error
	s.rootCtx, s.Shutdown = ctxutils.WithError(s.rootCtx, ssb.ErrShuttingDown)
	ctx := s.rootCtx

	r := repo.New(s.repoPath)

	// optionize?!
	s.RootLog, err = repo.OpenLog(r)
	if err != nil {
		return nil, errors.Wrap(err, "sbot: failed to open rootlog")
	}
	s.closers.addCloser(s.RootLog.(io.Closer))

	// TODO: rewirte about as consumer of msgs by type, like contacts
	// ab, serveAbouts, err := indexes.OpenAbout(kitlog.With(log, "index", "abouts"), r)
	// if err != nil {
	// 	return nil, errors.Wrap(err, "sbot: failed to open about idx")
	// }
	// // s.closers.addCloser(ab)
	// s.serveIndex(ctx, "abouts", serveAbouts)
	// s.AboutStore = ab

	if s.BlobStore == nil { // load default, local file blob store
		s.BlobStore, err = repo.OpenBlobStore(r)
		if err != nil {
			return nil, errors.Wrap(err, "sbot: failed to open blob store")
		}
	}

	// TODO: add flag to filter specific levels and/or units and pass nop to the others
	wantsLog := kitlog.With(log, "module", "WantManager")
	// wantsLog := kitlog.NewNopLogger()
	wm := blobstore.NewWantManager(wantsLog, s.BlobStore, s.eventCounter, s.systemGauge)
	s.WantManager = wm

	for _, opt := range s.lateInit {
		err := opt(s)
		if err != nil {
			return nil, errors.Wrap(err, "sbot: failed to apply late option")
		}
	}

	uf, ok := s.mlogIndicies[multilogs.IndexNameFeeds]
	if !ok {
		level.Warn(s.info).Log("event", "bot init", "msg", "loading default idx", "idx", multilogs.IndexNameFeeds)
		err = MountMultiLog(multilogs.IndexNameFeeds, multilogs.OpenUserFeeds)(s)
		if err != nil {
			return nil, errors.Wrap(err, "sbot: failed to open userFeeds index")
		}
		uf, ok = s.mlogIndicies[multilogs.IndexNameFeeds]
		if !ok {
			return nil, errors.Errorf("sbot: failed get loaded default index")
		}
	}

	if _, ok := s.simpleIndex["content-delete-requests"]; !ok {
		var dcrTrigger dropContentTrigger
		dcrTrigger.logger = kitlog.With(log, "module", "dcrTrigger")
		dcrTrigger.root = s.RootLog
		dcrTrigger.feeds = uf
		dcrTrigger.nuller = s
		err = MountSimpleIndex("content-delete-requests", dcrTrigger.MakeSimpleIndex)(s)
		if err != nil {
			return nil, errors.Wrap(err, "sbot: failed to open load default DCR index")
		}
	}

	var pubopts = []message.PublishOption{
		message.UseNowTimestamps(true),
	}
	if s.signHMACsecret != nil {
		pubopts = append(pubopts, message.SetHMACKey(s.signHMACsecret))
	}
	s.PublishLog, err = message.OpenPublishLog(s.RootLog, uf, s.KeyPair, pubopts...)
	if err != nil {
		return nil, errors.Wrap(err, "sbot: failed to create publish log")
	}

	// LogBuilder doesn't fully work yet
	if mt, ok := s.mlogIndicies["msgTypes"]; ok {
		level.Warn(s.info).Log("event", "bot init", "msg", "using experimental bytype:contact graph implementation")
		contactLog, err := mt.Get(librarian.Addr("contact"))
		if err != nil {
			return nil, errors.Wrap(err, "sbot: failed to open message contact sublog")
		}
		s.GraphBuilder, err = graph.NewLogBuilder(s.info, mutil.Indirect(s.RootLog, contactLog))
		if err != nil {
			return nil, errors.Wrap(err, "sbot: NewLogBuilder failed")
		}
	} else {
		gb, serveContacts, err := indexes.OpenContacts(kitlog.With(log, "module", "graph"), r)
		if err != nil {
			return nil, errors.Wrap(err, "sbot: OpenContacts failed")
		}
		s.serveIndex(ctx, "contacts", serveContacts)
		s.GraphBuilder = gb
	}

	// for i, n := range s.networks {

	// }

	// TODO: make plugabble
	// var peerPlug *peerinvites.Plugin
	// if mt, ok := s.mlogIndicies[multilogs.IndexNameFeeds]; ok {
	// 	peerPlug = peerinvites.New(kitlog.With(log, "plugin", "peerInvites"), s, mt, s.RootLog, s.PublishLog)
	// 	s.public.Register(peerPlug)
	// 	_, peerServ, err := peerPlug.OpenIndex(r)
	// 	if err != nil {
	// 		return nil, errors.Wrap(err, "sbot: failed to open about idx")
	// 	}
	// 	s.serveIndex(ctx, "contacts", peerServ)
	// }

	s.master.Register(publish.NewPlug(kitlog.With(log, "plugin", "publish"), s.PublishLog, s.RootLog))

	if pl, ok := s.mlogIndicies["privLogs"]; ok {
		userPrivs, err := pl.Get(s.KeyPair.Id.StoredAddr())
		if err != nil {
			return nil, errors.Wrap(err, "failed to open user private index")
		}
		s.master.Register(privplug.NewPlug(kitlog.With(log, "plugin", "private"), s.PublishLog, private.NewUnboxerLog(s.RootLog, userPrivs, s.KeyPair)))
	}

	// whoami
	whoami := whoami.New(kitlog.With(log, "plugin", "whoami"), s.KeyPair.Id)
	s.public.Register(whoami)
	s.master.Register(whoami)

	// blobs
	blobs := blobs.New(kitlog.With(log, "plugin", "blobs"), *s.KeyPair.Id, s.BlobStore, wm)
	s.public.Register(blobs)
	s.master.Register(blobs) // TODO: does not need to open a createWants on this one?!

	// names

	// outgoing gossip behavior
	var histOpts = []interface{}{
		gossip.HopCount(s.hopCount),
		gossip.Promisc(s.promisc),
	}

	if s.systemGauge != nil {
		histOpts = append(histOpts, s.systemGauge)
	}

	if s.eventCounter != nil {
		histOpts = append(histOpts, s.eventCounter)
	}

	if s.signHMACsecret != nil {
		var k [32]byte
		copy(k[:], s.signHMACsecret)
		histOpts = append(histOpts, gossip.HMACSecret(&k))
	}
	s.public.Register(gossip.New(ctx,
		kitlog.With(log, "plugin", "gossip"),
		s.KeyPair.Id, s.RootLog, uf, s.GraphBuilder,
		histOpts...))

	// incoming createHistoryStream handler
	hist := gossip.NewHist(ctx,
		kitlog.With(log, "plugin", "gossip/hist"),
		s.KeyPair.Id, s.RootLog, uf, s.GraphBuilder,
		histOpts...)
	s.public.Register(hist)

	s.master.Register(get.New(s))

	// raw log plugins
	s.master.Register(rawread.NewRXLog(s.RootLog)) // createLogStream
	s.master.Register(hist)                        // createHistoryStream

	s.master.Register(replicate.NewPlug(uf))

	// TODO: port to mountPlugin with network
	// s.master.Register(control.NewPlug(kitlog.With(log, "plugin", "ctrl"), s.Network))
	s.master.Register(status.New(s))

	return s, nil
}

func (s *Sbot) mkHandler() network.ConnToHandler {
	auth := s.GraphBuilder.Authorizer(s.KeyPair.Id, int(s.hopCount+2))
	return func(conn net.Conn) (muxrpc.Handler, error) {
		// bypassing badger-close bug to go through with an accept (or not) before closing the bot
		s.closedMu.Lock()
		defer s.closedMu.Unlock()

		remote, err := ssb.GetFeedRefFromAddr(conn.RemoteAddr())
		if err != nil {
			return nil, errors.Wrap(err, "sbot: expected an address containing an shs-bs addr")
		}
		if s.KeyPair.Id.Equal(remote) {
			return s.master.MakeHandler(conn)
		}

		// if peerPlug != nil {
		// 	if err := peerPlug.Authorize(remote); err == nil {
		// 		return peerPlug.Handler(), nil
		// 	}
		// }

		if s.promisc {
			return s.public.MakeHandler(conn)
		}
		if s.latency != nil {
			start := time.Now()
			defer func() {
				s.latency.With("part", "graph_auth").Observe(time.Since(start).Seconds())
			}()
		}
		err = auth.Authorize(remote)
		if err == nil {
			return s.public.MakeHandler(conn)
		}

		// shit - don't see a way to pass being a different feedtype with shs1
		// we also need to pass this up the stack...!
		remote.Algo = ssb.RefAlgoFeedGabby
		err = auth.Authorize(remote)
		if err == nil {
			return s.public.MakeHandler(conn)
		}
		return nil, err
	}
}
