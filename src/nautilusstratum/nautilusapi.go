package nautilusstratum

import (
	"context"
	"fmt"
	"time"

	"github.com/GRinvestPOOL/nautilus-stratum-bridge/src/gostratum"
	"github.com/Nautilus-Network/nautiliad/app/appmessage"
	"github.com/Nautilus-Network/nautiliad/infrastructure/network/rpcclient"
	"github.com/pkg/errors"
	"go.uber.org/zap"
)

type NautilusApi struct {
	address       string
	blockWaitTime time.Duration
	logger        *zap.SugaredLogger
	nautiliad      *rpcclient.RPCClient
	connected     bool
}

func NewNautilusApi(address string, blockWaitTime time.Duration, logger *zap.SugaredLogger) (*NautilusApi, error) {
	client, err := rpcclient.NewRPCClient(address)
	if err != nil {
		return nil, err
	}

	return &NautilusApi{
		address:       address,
		blockWaitTime: blockWaitTime,
		logger:        logger.With(zap.String("component", "nautilusapi:"+address)),
		nautiliad:      client,
		connected:     true,
	}, nil
}

func (ks *NautilusApi) Start(ctx context.Context, blockCb func()) {
	ks.waitForSync(true)
	go ks.startBlockTemplateListener(ctx, blockCb)
	go ks.startStatsThread(ctx)
}

func (ks *NautilusApi) startStatsThread(ctx context.Context) {
	ticker := time.NewTicker(30 * time.Second)
	for {
		select {
		case <-ctx.Done():
			ks.logger.Warn("context cancelled, stopping stats thread")
			return
		case <-ticker.C:
			dagResponse, err := ks.nautiliad.GetBlockDAGInfo()
			if err != nil {
				ks.logger.Warn("failed to get network hashrate from nautilus, prom stats will be out of date", zap.Error(err))
				continue
			}
			response, err := ks.nautiliad.EstimateNetworkHashesPerSecond(dagResponse.TipHashes[0], 1000)
			if err != nil {
				ks.logger.Warn("failed to get network hashrate from nautilus, prom stats will be out of date", zap.Error(err))
				continue
			}
			RecordNetworkStats(response.NetworkHashesPerSecond, dagResponse.BlockCount, dagResponse.Difficulty)
		}
	}
}

func (ks *NautilusApi) reconnect() error {
	if ks.nautiliad != nil {
		return ks.nautiliad.Reconnect()
	}

	client, err := rpcclient.NewRPCClient(ks.address)
	if err != nil {
		return err
	}
	ks.nautiliad = client
	return nil
}

func (s *NautilusApi) waitForSync(verbose bool) error {
	if verbose {
		s.logger.Info("checking nautiliad sync state")
	}
	for {
		clientInfo, err := s.nautiliad.GetInfo()
		if err != nil {
			return errors.Wrapf(err, "error fetching server info from nautiliad @ %s", s.address)
		}
		if clientInfo.IsSynced {
			break
		}
		s.logger.Warn("Karlsen is not synced, waiting for sync before starting bridge")
		time.Sleep(5 * time.Second)
	}
	if verbose {
		s.logger.Info("nautiliad synced, starting server")
	}
	return nil
}

func (s *NautilusApi) startBlockTemplateListener(ctx context.Context, blockReadyCb func()) {
	blockReadyChan := make(chan bool)
	err := s.nautiliad.RegisterForNewBlockTemplateNotifications(func(_ *appmessage.NewBlockTemplateNotificationMessage) {
		blockReadyChan <- true
	})
	if err != nil {
		s.logger.Error("fatal: failed to register for block notifications from nautilus")
	}

	ticker := time.NewTicker(s.blockWaitTime)
	for {
		if err := s.waitForSync(false); err != nil {
			s.logger.Error("error checking nautiliad sync state, attempting reconnect: ", err)
			if err := s.reconnect(); err != nil {
				s.logger.Error("error reconnecting to nautiliad, waiting before retry: ", err)
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

func (ks *NautilusApi) GetBlockTemplate(
	client *gostratum.StratumContext) (*appmessage.GetBlockTemplateResponseMessage, error) {
	template, err := ks.nautiliad.GetBlockTemplate(client.WalletAddr,
		fmt.Sprintf(`'%s' via nautilus-network/nautilus-stratum-bridge_%s`, client.RemoteApp, version))
	if err != nil {
		return nil, errors.Wrap(err, "failed fetching new block template from nautilus")
	}
	return template, nil
}
