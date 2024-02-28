package topiastratum

import (
	"context"
	"fmt"
	"time"

	"github.com/fabbez/topiad/app/appmessage"
	"github.com/fabbez/topiad/infrastructure/network/rpcclient"
	"github.com/fabbez/topiastratum/src/gostratum"
	"github.com/pkg/errors"
	"go.uber.org/zap"
)

type topiaApi struct {
	address       string
	blockWaitTime time.Duration
	logger        *zap.SugaredLogger
	topiad        *rpcclient.RPCClient
	connected     bool
}

func NewtopiaAPI(address string, blockWaitTime time.Duration, logger *zap.SugaredLogger) (*topiaApi, error) {
	client, err := rpcclient.NewRPCClient(address)
	if err != nil {
		return nil, err
	}

	return &topiaApi{
		address:       address,
		blockWaitTime: blockWaitTime,
		logger:        logger.With(zap.String("component", "topiaapi:"+address)),
		topiad:        client,
		connected:     true,
	}, nil
}

func (ks *topiaApi) Start(ctx context.Context, blockCb func()) {
	ks.waitForSync(true)
	go ks.startBlockTemplateListener(ctx, blockCb)
	go ks.startStatsThread(ctx)
}

func (ks *topiaApi) startStatsThread(ctx context.Context) {
	ticker := time.NewTicker(30 * time.Second)
	for {
		select {
		case <-ctx.Done():
			ks.logger.Warn("context cancelled, stopping stats thread")
			return
		case <-ticker.C:
			dagResponse, err := ks.topiad.GetBlockDAGInfo()
			if err != nil {
				ks.logger.Warn("failed to get network hashrate from topia, prom stats will be out of date", zap.Error(err))
				continue
			}
			response, err := ks.kaspad.EstimateNetworkHashesPerSecond(dagResponse.TipHashes[0], 1000)
			if err != nil {
				ks.logger.Warn("failed to get network hashrate from topia, prom stats will be out of date", zap.Error(err))
				continue
			}
			RecordNetworkStats(response.NetworkHashesPerSecond, dagResponse.BlockCount, dagResponse.Difficulty)
		}
	}
}

func (ks *topiaApi) reconnect() error {
	if ks.kaspad != nil {
		return ks.topiad.Reconnect()
	}

	client, err := rpcclient.NewRPCClient(ks.address)
	if err != nil {
		return err
	}
	ks.topiad = client
	return nil
}

func (s *topiaApi) waitForSync(verbose bool) error {
	if verbose {
		s.logger.Info("checking topiad sync state")
	}
	for {
		clientInfo, err := s.topiad.GetInfo()
		if err != nil {
			return errors.Wrapf(err, "error fetching server info from topiad @ %s", s.address)
		}
		if clientInfo.IsSynced {
			break
		}
		s.logger.Warn("topia is not synced, waiting for sync before starting bridge")
		time.Sleep(5 * time.Second)
	}
	if verbose {
		s.logger.Info("topiad synced, starting server")
	}
	return nil
}

func (s *topiaApi) startBlockTemplateListener(ctx context.Context, blockReadyCb func()) {
	blockReadyChan := make(chan bool)
	err := s.topiad.RegisterForNewBlockTemplateNotifications(func(_ *appmessage.NewBlockTemplateNotificationMessage) {
		blockReadyChan <- true
	})
	if err != nil {
		s.logger.Error("fatal: failed to register for block notifications from topia")
	}

	ticker := time.NewTicker(s.blockWaitTime)
	for {
		if err := s.waitForSync(false); err != nil {
			s.logger.Error("error checking topiad sync state, attempting reconnect: ", err)
			if err := s.reconnect(); err != nil {
				s.logger.Error("error reconnecting to topiad, waiting before retry: ", err)
				time.Sleep(5 * time.Second)
			}
		}
		select {
		case <-ctx.Done():
			s.logger.Warn("context cancelled, stopping block update listener")
			return
		case <-blockReadyChan:
			blockReadyCb()
			ticker.Reset(s.blockWaitTime)
		case <-ticker.C: // timeout, manually check for new blocks
			blockReadyCb()
		}
	}
}

func (ks *topiaApi) GetBlockTemplate(
	client *gostratum.StratumContext) (*appmessage.GetBlockTemplateResponseMessage, error) {
	template, err := ks.topiad.GetBlockTemplate(client.WalletAddr,
		fmt.Sprintf(`'%s' via onemorebsmith/kaspa-stratum-bridge_%s`, client.RemoteApp, version))
	if err != nil {
		return nil, errors.Wrap(err, "failed fetching new block template from topia")
	}
	return template, nil
}
