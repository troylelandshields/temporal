// Copyright (c) 2019 Uber Technologies, Inc.
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in
// all copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN
// THE SOFTWARE.

package history

import (
	ctx "context"
	"fmt"
	"time"

	"github.com/uber/cadence/.gen/go/shared"
	"github.com/uber/cadence/common/persistence"
)

type (
	nDCTransactionMgrForNewWorkflow interface {
		dispatchForNewWorkflow(
			ctx ctx.Context,
			now time.Time,
			targetWorkflow nDCWorkflow,
		) error
	}

	nDCTransactionMgrForNewWorkflowImpl struct {
		transactionMgr *nDCTransactionMgrImpl
	}
)

func newNDCTransactionMgrForNewWorkflow(
	transactionMgr *nDCTransactionMgrImpl,
) *nDCTransactionMgrForNewWorkflowImpl {

	return &nDCTransactionMgrForNewWorkflowImpl{
		transactionMgr: transactionMgr,
	}
}

func (r *nDCTransactionMgrForNewWorkflowImpl) dispatchForNewWorkflow(
	ctx ctx.Context,
	now time.Time,
	targetWorkflow nDCWorkflow,
) error {

	// NOTE: this function does NOT mutate current workflow or target workflow,
	//  workflow mutation is done in methods within executeTransaction function

	targetExecutionInfo := targetWorkflow.getMutableState().GetExecutionInfo()
	domainID := targetExecutionInfo.DomainID
	workflowID := targetExecutionInfo.WorkflowID
	targetRunID := targetExecutionInfo.RunID

	// we need to check the current workflow execution
	currentRunID, err := r.transactionMgr.getCurrentWorkflowRunID(
		ctx,
		domainID,
		workflowID,
	)
	if err != nil || currentRunID == targetRunID {
		// error out or workflow already created
		return err
	}

	if currentRunID == "" {
		// current record does not exists
		return r.executeTransaction(
			ctx,
			now,
			nDCTransactionPolicyCreateAsCurrent,
			nil,
			targetWorkflow,
		)
	}

	// there exists a current workflow, need additional check
	currentWorkflow, err := loadNDCWorkflow(
		ctx,
		r.transactionMgr.domainCache,
		r.transactionMgr.historyCache,
		r.transactionMgr.clusterMetadata,
		domainID,
		workflowID,
		currentRunID,
	)
	if err != nil {
		return err
	}

	targetWorkflowIsNewer, err := targetWorkflow.happensAfter(currentWorkflow)
	if err != nil {
		return err
	}

	if !targetWorkflowIsNewer {
		// target workflow is older than current workflow, need to suppress the target workflow
		return r.executeTransaction(
			ctx,
			now,
			nDCTransactionPolicyCreateAsZombie,
			currentWorkflow,
			targetWorkflow,
		)
	}

	// target workflow is newer than current workflow
	if !currentWorkflow.getMutableState().IsWorkflowExecutionRunning() {
		// current workflow is completed
		// proceed to create workflow
		return r.executeTransaction(
			ctx,
			now,
			nDCTransactionPolicyCreateAsCurrent,
			currentWorkflow,
			targetWorkflow,
		)
	}

	// current workflow is still running, need to suppress the current workflow
	return r.executeTransaction(
		ctx,
		now,
		nDCTransactionPolicySuppressCurrentAndCreateAsCurrent,
		currentWorkflow,
		targetWorkflow,
	)
}

func (r *nDCTransactionMgrForNewWorkflowImpl) createAsCurrent(
	ctx ctx.Context,
	now time.Time,
	currentWorkflow nDCWorkflow,
	targetWorkflow nDCWorkflow,
) error {

	targetWorkflowSnapshot, targetWorkflowEventsSeq, err := targetWorkflow.getMutableState().CloseTransactionAsSnapshot(
		now,
		transactionPolicyPassive,
	)
	if err != nil {
		return err
	}

	targetWorkflowHistorySize, err := targetWorkflow.getContext().persistFirstWorkflowEvents(
		targetWorkflowEventsSeq[0],
	)
	if err != nil {
		return err
	}

	// target workflow to be created as current
	if currentWorkflow != nil {
		// current workflow exists, need to do compare and swap
		createMode := persistence.CreateWorkflowModeWorkflowIDReuse
		prevRunID := currentWorkflow.getMutableState().GetExecutionInfo().RunID
		prevLastWriteVersion, _, err := currentWorkflow.getVectorClock()
		if err != nil {
			return err
		}
		return targetWorkflow.getContext().createWorkflowExecution(
			targetWorkflowSnapshot, targetWorkflowHistorySize, now,
			createMode, prevRunID, prevLastWriteVersion,
		)
	}

	// current workflow does not exists, create as brand new
	createMode := persistence.CreateWorkflowModeBrandNew
	prevRunID := ""
	prevLastWriteVersion := int64(0)
	return targetWorkflow.getContext().createWorkflowExecution(
		targetWorkflowSnapshot, targetWorkflowHistorySize, now,
		createMode, prevRunID, prevLastWriteVersion,
	)
}

func (r *nDCTransactionMgrForNewWorkflowImpl) createAsZombie(
	ctx ctx.Context,
	now time.Time,
	currentWorkflow nDCWorkflow,
	targetWorkflow nDCWorkflow,
) error {

	if err := targetWorkflow.suppressWorkflowBy(
		currentWorkflow,
	); err != nil {
		return err
	}

	targetWorkflowSnapshot, targetWorkflowEventsSeq, err := targetWorkflow.getMutableState().CloseTransactionAsSnapshot(
		now,
		transactionPolicyPassive,
	)
	if err != nil {
		return err
	}

	targetWorkflowHistorySize, err := targetWorkflow.getContext().persistFirstWorkflowEvents(
		targetWorkflowEventsSeq[0],
	)
	if err != nil {
		return err
	}

	createMode := persistence.CreateWorkflowModeZombie
	prevRunID := ""
	prevLastWriteVersion := int64(0)
	return targetWorkflow.getContext().createWorkflowExecution(
		targetWorkflowSnapshot, targetWorkflowHistorySize, now,
		createMode, prevRunID, prevLastWriteVersion,
	)
}

func (r *nDCTransactionMgrForNewWorkflowImpl) suppressCurrentAndCreateAsCurrent(
	ctx ctx.Context,
	now time.Time,
	currentWorkflow nDCWorkflow,
	targetWorkflow nDCWorkflow,
) error {

	if err := currentWorkflow.suppressWorkflowBy(
		targetWorkflow,
	); err != nil {
		return err
	}

	return currentWorkflow.getContext().updateWorkflowExecutionWithNew(
		now,
		targetWorkflow.getContext(),
		targetWorkflow.getMutableState(),
		transactionPolicyActive,
		transactionPolicyPassive.ptr(),
	)
}

func (r *nDCTransactionMgrForNewWorkflowImpl) executeTransaction(
	ctx ctx.Context,
	now time.Time,
	transactionPolicy nDCTransactionPolicy,
	currentWorkflow nDCWorkflow,
	targetWorkflow nDCWorkflow,
) (retError error) {

	defer func() {
		r.cleanupTransaction(currentWorkflow, targetWorkflow, retError)
	}()

	switch transactionPolicy {
	case nDCTransactionPolicyCreateAsCurrent:
		return r.createAsCurrent(
			ctx,
			now,
			currentWorkflow,
			targetWorkflow,
		)

	case nDCTransactionPolicyCreateAsZombie:
		return r.createAsZombie(
			ctx,
			now,
			currentWorkflow,
			targetWorkflow,
		)

	case nDCTransactionPolicySuppressCurrentAndCreateAsCurrent:
		return r.suppressCurrentAndCreateAsCurrent(
			ctx,
			now,
			currentWorkflow,
			targetWorkflow,
		)

	default:
		return &shared.InternalServiceError{
			Message: fmt.Sprintf("nDCTransactionMgr encounter unknown transaction type: %v", transactionPolicy),
		}
	}
}

func (r *nDCTransactionMgrForNewWorkflowImpl) cleanupTransaction(
	currentWorkflow nDCWorkflow,
	targetWorkflow nDCWorkflow,
	err error,
) {

	if currentWorkflow != nil {
		currentWorkflow.getReleaseFn()(err)
	}
	if targetWorkflow != nil {
		targetWorkflow.getReleaseFn()(err)
	}
}
