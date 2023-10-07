package ceremony

import (
	"bytes"
	"context"
	"time"

	"github.com/pkg/errors"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/anypb"
	"source.quilibrium.com/quilibrium/monorepo/go-libp2p-blossomsub/pb"
	"source.quilibrium.com/quilibrium/monorepo/node/execution/ceremony/application"
	"source.quilibrium.com/quilibrium/monorepo/node/p2p"
	"source.quilibrium.com/quilibrium/monorepo/node/protobufs"
	"source.quilibrium.com/quilibrium/monorepo/node/store"
)

func (e *CeremonyDataClockConsensusEngine) handleSync(
	message *pb.Message,
) error {
	e.logger.Debug(
		"received message",
		zap.Binary("data", message.Data),
		zap.Binary("from", message.From),
		zap.Binary("signature", message.Signature),
	)
	if bytes.Equal(message.From, e.pubSub.GetPeerID()) {
		return nil
	}

	msg := &protobufs.Message{}

	if err := proto.Unmarshal(message.Data, msg); err != nil {
		return errors.Wrap(err, "handle sync")
	}

	any := &anypb.Any{}
	if err := proto.Unmarshal(msg.Payload, any); err != nil {
		return errors.Wrap(err, "handle sync")
	}

	switch any.TypeUrl {
	case protobufs.ProvingKeyAnnouncementType:
		if err := e.handleProvingKey(
			message.From,
			msg.Address,
			any,
		); err != nil {
			return errors.Wrap(err, "handle sync")
		}
	case protobufs.KeyBundleAnnouncementType:
		if err := e.handleKeyBundle(
			message.From,
			msg.Address,
			any,
		); err != nil {
			return errors.Wrap(err, "handle sync")
		}
	}

	return nil
}

// GetCompressedSyncFrames implements protobufs.CeremonyServiceServer.
func (e *CeremonyDataClockConsensusEngine) GetCompressedSyncFrames(
	request *protobufs.ClockFramesRequest,
	server protobufs.CeremonyService_GetCompressedSyncFramesServer,
) error {
	e.logger.Info(
		"received clock frame request",
		zap.Uint64("from_frame_number", request.FromFrameNumber),
		zap.Uint64("to_frame_number", request.ToFrameNumber),
	)

	from := request.FromFrameNumber

	_, _, err := e.clockStore.GetDataClockFrame(
		request.Filter,
		from,
	)
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			e.logger.Error(
				"peer asked for frame that returned error",
				zap.Uint64("frame_number", request.FromFrameNumber),
			)
			return errors.Wrap(err, "get compressed sync frames")
		} else {
			e.logger.Debug(
				"peer asked for undiscovered frame",
				zap.Uint64("frame_number", request.FromFrameNumber),
			)

			if err := server.SendMsg(
				&protobufs.ClockFramesResponse{
					Filter:          request.Filter,
					FromFrameNumber: 0,
					ToFrameNumber:   0,
					ClockFrames:     []*protobufs.ClockFrame{},
				},
			); err != nil {
				return errors.Wrap(err, "get compressed sync frames")
			}

			return nil
		}
	}

	max := e.frame
	to := request.ToFrameNumber

	for {
		if to == 0 || to-from > 32 {
			if max > from+31 {
				to = from + 31
			} else {
				to = max
			}
		}

		syncMsg, err := e.clockStore.GetCompressedDataClockFrames(
			e.filter,
			from,
			to,
		)
		if err != nil {
			return errors.Wrap(err, "get compressed sync frames")
		}

		if err := server.SendMsg(syncMsg); err != nil {
			return errors.Wrap(err, "get compressed sync frames")
		}

		if (request.ToFrameNumber == 0 || request.ToFrameNumber > to) && max > to {
			from = to + 1
			if request.ToFrameNumber > to {
				to = request.ToFrameNumber
			} else {
				to = 0
			}
		} else {
			break
		}
	}

	return nil
}

func (e *CeremonyDataClockConsensusEngine) decompressAndStoreCandidates(
	syncMsg *protobufs.CeremonyCompressedSync,
) error {
	for _, frame := range syncMsg.TruncatedClockFrames {
		commits := (len(frame.Input) - 516) / 74
		e.logger.Info(
			"processing frame",
			zap.Uint64("frame_number", frame.FrameNumber),
			zap.Int("aggregate_commits", commits),
		)
		for j := 0; j < commits; j++ {
			e.logger.Info(
				"processing commit",
				zap.Uint64("frame_number", frame.FrameNumber),
				zap.Int("commit_index", j),
			)
			commit := frame.Input[516+(j*74) : 516+((j+1)*74)]
			var aggregateProof *protobufs.InclusionProofsMap
			for _, a := range syncMsg.Proofs {
				if bytes.Equal(a.FrameCommit, commit) {
					e.logger.Info(
						"found matching proof",
						zap.Uint64("frame_number", frame.FrameNumber),
						zap.Int("commit_index", j),
					)
					aggregateProof = a
					break
				}
			}
			if aggregateProof == nil {
				e.logger.Error(
					"could not find matching proof",
					zap.Uint64("frame_number", frame.FrameNumber),
					zap.Int("commit_index", j),
					zap.Binary("proof", aggregateProof.Proof),
				)
				return errors.Wrap(
					store.ErrInvalidData,
					"decompress and store candidates",
				)
			}
			inc := &protobufs.InclusionAggregateProof{
				Filter:               e.filter,
				FrameNumber:          frame.FrameNumber,
				InclusionCommitments: []*protobufs.InclusionCommitment{},
				Proof:                aggregateProof.Proof,
			}

			for k, c := range aggregateProof.Commitments {
				e.logger.Info(
					"adding inclusion commitment",
					zap.Uint64("frame_number", frame.FrameNumber),
					zap.Int("commit_index", j),
					zap.Int("inclusion_commit_index", k),
					zap.String("type_url", c.TypeUrl),
				)
				incCommit := &protobufs.InclusionCommitment{
					Filter:      e.filter,
					FrameNumber: frame.FrameNumber,
					Position:    uint32(k),
					TypeUrl:     c.TypeUrl,
					Data:        []byte{},
					Commitment:  c.Commitment,
				}
				var output *protobufs.IntrinsicExecutionOutput
				if c.TypeUrl == protobufs.IntrinsicExecutionOutputType {
					output = &protobufs.IntrinsicExecutionOutput{}
				}
				for l, h := range c.SegmentHashes {
					for _, s := range syncMsg.Segments {
						if bytes.Equal(s.Hash, h) {
							if output != nil {
								if l == 0 {
									e.logger.Info(
										"found first half of matching segment data",
										zap.Uint64("frame_number", frame.FrameNumber),
										zap.Int("commit_index", j),
										zap.Int("inclusion_commit_index", k),
										zap.String("type_url", c.TypeUrl),
									)
									output.Address = s.Data[:32]
									output.Output = s.Data[32:]
								} else {
									e.logger.Info(
										"found second half of matching segment data",
										zap.Uint64("frame_number", frame.FrameNumber),
										zap.Int("commit_index", j),
										zap.Int("inclusion_commit_index", k),
										zap.String("type_url", c.TypeUrl),
									)
									output.Proof = s.Data
									b, err := proto.Marshal(output)
									if err != nil {
										return errors.Wrap(err, "decompress and store candidates")
									}
									incCommit.Data = b
									break
								}
							} else {
								e.logger.Info(
									"found matching segment data",
									zap.Uint64("frame_number", frame.FrameNumber),
									zap.Int("commit_index", j),
									zap.Int("inclusion_commit_index", k),
									zap.String("type_url", c.TypeUrl),
								)
								incCommit.Data = append(incCommit.Data, s.Data...)
								break
							}
						}
					}
				}
				inc.InclusionCommitments = append(
					inc.InclusionCommitments,
					incCommit,
				)
			}

			frame.AggregateProofs = append(
				frame.AggregateProofs,
				inc,
			)
		}

		f, err := proto.Marshal(frame)
		if err != nil {
			return errors.Wrap(err, "decompress and store candidates")
		}

		any := &anypb.Any{
			TypeUrl: protobufs.ClockFrameType,
			Value:   f,
		}
		if err = e.handleClockFrameData(
			e.syncingTarget,
			application.CEREMONY_ADDRESS,
			any,
			true,
		); err != nil {
			return errors.Wrap(err, "decompress and store candidates")
		}
	}

	e.logger.Info(
		"decompressed and stored sync for range",
		zap.Uint64("from", syncMsg.FromFrameNumber),
		zap.Uint64("to", syncMsg.ToFrameNumber),
	)
	return nil
}

type svr struct {
	protobufs.UnimplementedCeremonyServiceServer
	svrChan chan protobufs.CeremonyService_GetPublicChannelServer
}

func (e *svr) GetCompressedSyncFrames(
	request *protobufs.ClockFramesRequest,
	server protobufs.CeremonyService_GetCompressedSyncFramesServer,
) error {
	return errors.New("not supported")
}

func (e *svr) GetPublicChannel(
	server protobufs.CeremonyService_GetPublicChannelServer,
) error {
	go func() {
		e.svrChan <- server
	}()
	<-server.Context().Done()
	return nil
}

func (e *CeremonyDataClockConsensusEngine) GetPublicChannelForProvingKey(
	initiator bool,
	provingKey []byte,
) (p2p.PublicChannelClient, error) {
	if initiator {
		svrChan := make(
			chan protobufs.CeremonyService_GetPublicChannelServer,
		)
		after := time.After(20 * time.Second)
		go func() {
			server := grpc.NewServer(
				grpc.MaxSendMsgSize(400*1024*1024),
				grpc.MaxRecvMsgSize(400*1024*1024),
			)

			s := &svr{
				svrChan: svrChan,
			}
			protobufs.RegisterCeremonyServiceServer(server, s)

			if err := e.pubSub.StartDirectChannelListener(
				provingKey,
				server,
			); err != nil {
				e.logger.Error(
					"could not get public channel for proving key",
					zap.Error(err),
				)
				svrChan <- nil
			}
		}()
		select {
		case s := <-svrChan:
			return s, nil
		case <-after:
			return nil, errors.Wrap(
				errors.New("timed out"),
				"get public channel for proving key",
			)
		}
	} else {
		cc, err := e.pubSub.GetDirectChannel(provingKey)
		if err != nil {
			e.logger.Error(
				"could not get public channel for proving key",
				zap.Error(err),
			)
			return nil, nil
		}
		client := protobufs.NewCeremonyServiceClient(cc)
		s, err := client.GetPublicChannel(
			context.Background(),
			grpc.MaxCallSendMsgSize(400*1024*1024),
			grpc.MaxCallRecvMsgSize(400*1024*1024),
		)
		return s, errors.Wrap(err, "get public channel for proving key")
	}
}

// GetPublicChannel implements protobufs.CeremonyServiceServer.
func (e *CeremonyDataClockConsensusEngine) GetPublicChannel(
	server protobufs.CeremonyService_GetPublicChannelServer,
) error {
	return errors.New("not supported")
}

func (e *CeremonyDataClockConsensusEngine) handleProvingKeyRequest(
	peerID []byte,
	address []byte,
	any *anypb.Any,
) error {
	if bytes.Equal(peerID, e.pubSub.GetPeerID()) {
		return nil
	}

	request := &protobufs.ProvingKeyRequest{}
	if err := any.UnmarshalTo(request); err != nil {
		return nil
	}

	if len(request.ProvingKeyBytes) == 0 {
		e.logger.Debug(
			"received proving key request for empty key",
			zap.Binary("peer_id", peerID),
			zap.Binary("address", address),
		)
		return nil
	}

	e.pubSub.Subscribe(
		append(append([]byte{}, e.filter...), peerID...),
		e.handleSync,
		true,
	)

	e.logger.Debug(
		"received proving key request",
		zap.Binary("peer_id", peerID),
		zap.Binary("address", address),
		zap.Binary("proving_key", request.ProvingKeyBytes),
	)

	var provingKey *protobufs.ProvingKeyAnnouncement
	inclusion, err := e.keyStore.GetProvingKey(request.ProvingKeyBytes)
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			e.logger.Debug(
				"peer asked for proving key that returned error",
				zap.Binary("peer_id", peerID),
				zap.Binary("address", address),
				zap.Binary("proving_key", request.ProvingKeyBytes),
			)
			return nil
		}

		provingKey, err = e.keyStore.GetStagedProvingKey(request.ProvingKeyBytes)
		if !errors.Is(err, store.ErrNotFound) {
			e.logger.Debug(
				"peer asked for proving key that returned error",
				zap.Binary("peer_id", peerID),
				zap.Binary("address", address),
				zap.Binary("proving_key", request.ProvingKeyBytes),
			)
			return nil
		} else if err != nil {
			e.logger.Debug(
				"peer asked for unknown proving key",
				zap.Binary("peer_id", peerID),
				zap.Binary("address", address),
				zap.Binary("proving_key", request.ProvingKeyBytes),
			)
			return nil
		}
	} else {
		err := proto.Unmarshal(inclusion.Data, provingKey)
		if err != nil {
			e.logger.Debug(
				"inclusion commitment could not be deserialized",
				zap.Binary("peer_id", peerID),
				zap.Binary("address", address),
				zap.Binary("proving_key", request.ProvingKeyBytes),
			)
			return nil
		}
	}

	if err := e.publishMessage(
		append(append([]byte{}, e.filter...), peerID...),
		provingKey,
	); err != nil {
		return nil
	}

	return nil
}
