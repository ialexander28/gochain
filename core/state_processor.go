// Copyright 2015 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// The go-ethereum library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The go-ethereum library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the go-ethereum library. If not, see <http://www.gnu.org/licenses/>.

package core

import (
	"context"

	"github.com/gochain-io/gochain/common/perfutils"
	"github.com/gochain-io/gochain/consensus"
	"github.com/gochain-io/gochain/core/state"
	"github.com/gochain-io/gochain/core/types"
	"github.com/gochain-io/gochain/core/vm"
	"github.com/gochain-io/gochain/crypto"
	"github.com/gochain-io/gochain/params"
)

// StateProcessor is a basic Processor, which takes care of transitioning
// state from one point to another.
//
// StateProcessor implements Processor.
type StateProcessor struct {
	config *params.ChainConfig // Chain configuration options
	bc     *BlockChain         // Canonical block chain
	engine consensus.Engine    // Consensus engine used for block rewards
}

// NewStateProcessor initialises a new StateProcessor.
func NewStateProcessor(config *params.ChainConfig, bc *BlockChain, engine consensus.Engine) *StateProcessor {
	return &StateProcessor{
		config: config,
		bc:     bc,
		engine: engine,
	}
}

// Process processes the state changes according to the Ethereum rules by running
// the transaction messages using the statedb and applying any rewards to both
// the processor (coinbase) and any included uncles.
//
// Process returns the receipts and logs accumulated during the process and
// returns the amount of gas that was used in the process. If any of the
// transactions failed to execute due to insufficient gas it will return an error.
func (p *StateProcessor) Process(ctx context.Context, block *types.Block, statedb *state.StateDB, cfg vm.Config) (types.Receipts, []*types.Log, uint64, error) {
	txs := block.Transactions()
	header := block.Header()

	var (
		receipts = make(types.Receipts, len(txs))
		usedGas  = new(uint64)
		allLogs  []*types.Log
		gp       = new(GasPool).AddGas(block.GasLimit())
	)

	perfTimer := perfutils.GetTimer(ctx)

	// Create a new emv context and environment.
	evmContext := NewEVMContextLite(header, p.bc, nil)
	vmenv := vm.NewEVM(evmContext, statedb, p.config, cfg)

	// Iterate over and process the individual transactions
	for i, tx := range txs {
		ps := perfTimer.Start(perfutils.StatedbPrepare)
		statedb.Prepare(tx.Hash(), block.Hash(), i)
		ps.Stop()
		ps = perfTimer.Start(perfutils.ApplyTransaction)
		receipt, _, err := ApplyTransaction(ctx, vmenv, p.config, gp, statedb, header, tx, usedGas, types.MakeSigner(p.config, header.Number))
		if err != nil {
			return nil, nil, 0, err
		}
		ps.Stop()
		receipts[i] = receipt
		allLogs = append(allLogs, receipt.Logs...)
	}
	// Finalize the block, applying any consensus engine specific extras (e.g. block rewards)
	ps := perfTimer.Start(perfutils.Finalize)
	_ = p.engine.Finalize(ctx, p.bc, header, statedb, block.Transactions(), block.Uncles(), receipts, false)
	ps.Stop()

	return receipts, allLogs, *usedGas, nil
}

// ApplyTransaction attempts to apply a transaction to the given state database
// and uses the input parameters for its environment. It returns the receipt
// for the transaction, gas used and an error if the transaction failed,
// indicating the block was invalid.
func ApplyTransaction(ctx context.Context, vmenv *vm.EVM, config *params.ChainConfig, gp *GasPool, statedb *state.StateDB, header *types.Header, tx *types.Transaction, usedGas *uint64, signer types.Signer) (*types.Receipt, uint64, error) {
	perfTimer := perfutils.GetTimer(ctx)
	ps := perfTimer.Start(perfutils.TxAsMessage)
	msg, err := tx.AsMessage(ctx, signer)
	if err != nil {
		return nil, 0, err
	}
	ps.Stop()

	vmenv.Context.Origin = msg.From()
	vmenv.Context.GasPrice = msg.GasPrice()
	vmenv.Reset()

	// Apply the transaction to the current state (included in the env)
	ps = perfTimer.Start(perfutils.ApplyMessage)
	_, gas, failed, err := ApplyMessage(vmenv, msg, gp)
	ps.Stop()
	if err != nil {
		return nil, 0, err
	}
	// Update the state with pending changes
	var root []byte
	if config.IsByzantium(header.Number) {
		ps = perfTimer.Start(perfutils.StatedbFinalize)
		statedb.Finalise(true)
		ps.Stop()
	} else {
		ps = perfTimer.Start(perfutils.IntermediateRoot)
		root = statedb.IntermediateRoot(config.IsEIP158(header.Number)).Bytes()
		ps.Stop()
	}
	*usedGas += gas

	// Create a new receipt for the transaction, storing the intermediate root and gas used by the tx
	// based on the eip phase, we're passing wether the root touch-delete accounts.
	ps = perfTimer.Start(perfutils.NewReceipt)
	receipt := types.NewReceipt(root, failed, *usedGas)
	receipt.TxHash = tx.Hash()
	ps.Stop()
	receipt.GasUsed = gas
	// if the transaction created a contract, store the creation address in the receipt.
	if msg.To() == nil {
		receipt.ContractAddress = crypto.CreateAddress(vmenv.Context.Origin, tx.Nonce())
	}
	// Set the receipt logs and create a bloom for filtering
	ps = perfTimer.Start(perfutils.GetLogs)
	receipt.Logs = statedb.GetLogs(tx.Hash())
	ps.Stop()
	ps = perfTimer.Start(perfutils.CreateBloom)
	receipt.Bloom = types.CreateBloom(types.Receipts{receipt})
	ps.Stop()

	return receipt, gas, err
}
