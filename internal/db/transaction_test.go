package db

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// MockTransaction simulates database transaction behavior for testing
type MockTransaction struct {
	shouldFailCommit bool
	shouldFailExec   bool
	shouldFailBegin  bool
	execCalls        []string
	commitCalls      int
	rollbackCalls    int
	isCommitted      bool
	isRolledBack     bool
}

func (mt *MockTransaction) Begin() error {
	if mt.shouldFailBegin {
		return errors.New("mock begin failure")
	}
	return nil
}

func (mt *MockTransaction) Exec(query string, args ...interface{}) error {
	mt.execCalls = append(mt.execCalls, query)
	if mt.shouldFailExec {
		return errors.New("mock exec failure")
	}
	return nil
}

func (mt *MockTransaction) Commit() error {
	mt.commitCalls++
	if mt.shouldFailCommit {
		return errors.New("mock commit failure")
	}
	mt.isCommitted = true
	return nil
}

func (mt *MockTransaction) Rollback() error {
	mt.rollbackCalls++
	mt.isRolledBack = true
	return nil
}

// MockDB simulates database behavior for testing atomic transactions
type MockDB struct {
	mu           sync.Mutex
	transactions []*MockTransaction
	currentTx    int
	log          MockLogger
}

type MockLogger struct {
	mu            sync.Mutex
	debugMessages []string
	errorMessages []string
}

func (ml *MockLogger) Debug(msg string, args ...interface{}) {
	ml.mu.Lock()
	defer ml.mu.Unlock()
	ml.debugMessages = append(ml.debugMessages, msg)
}

func (ml *MockLogger) Error(msg string, args ...interface{}) {
	ml.mu.Lock()
	defer ml.mu.Unlock()
	ml.errorMessages = append(ml.errorMessages, msg)
}

func (ml *MockLogger) Info(msg string, args ...interface{}) {}
func (ml *MockLogger) Warn(msg string, args ...interface{}) {}

func (ml *MockLogger) GetDebugMessages() []string {
	ml.mu.Lock()
	defer ml.mu.Unlock()
	// Return a copy to avoid race conditions
	messages := make([]string, len(ml.debugMessages))
	copy(messages, ml.debugMessages)
	return messages
}

func NewMockDB() *MockDB {
	return &MockDB{
		transactions: make([]*MockTransaction, 0),
		log:          MockLogger{},
	}
}

func (mdb *MockDB) AddTransaction(tx *MockTransaction) {
	mdb.transactions = append(mdb.transactions, tx)
}

func (mdb *MockDB) simulateProcessTranscriptionCorrection(ctx context.Context, filePath string, correctionFunc func() (string, map[string]interface{}, error)) error {
	// This simulates the atomic transaction pattern from ProcessTranscriptionCorrection

	// Start first transaction for status update
	mdb.mu.Lock()
	if mdb.currentTx >= len(mdb.transactions) {
		mdb.mu.Unlock()
		return errors.New("no more mock transactions available")
	}

	tx1 := mdb.transactions[mdb.currentTx]
	mdb.currentTx++
	mdb.mu.Unlock()

	if err := tx1.Begin(); err != nil {
		return err
	}

	// Set status to in_progress
	if err := tx1.Exec("UPDATE transcriptions SET correction_status = 'in_progress' WHERE file_path = $1", filePath); err != nil {
		if rollbackErr := tx1.Rollback(); rollbackErr != nil {
			mdb.log.Error("Failed to rollback transaction: %v", rollbackErr)
		}
		return err
	}

	// Commit the status update
	if err := tx1.Commit(); err != nil {
		return err
	}

	// Perform the correction (this may take a long time)
	correctedText, _, correctionErr := correctionFunc()

	// Start second transaction for final update
	mdb.mu.Lock()
	if mdb.currentTx >= len(mdb.transactions) {
		mdb.mu.Unlock()
		return errors.New("no more mock transactions available")
	}

	tx2 := mdb.transactions[mdb.currentTx]
	mdb.currentTx++
	mdb.mu.Unlock()

	if err := tx2.Begin(); err != nil {
		return err
	}

	if correctionErr != nil {
		// Update with error status
		if err := tx2.Exec("UPDATE transcriptions SET correction_status = 'failed' WHERE file_path = $1", filePath); err != nil {
			mdb.log.Error("Failed to update failed correction status")
		} else {
			if commitErr := tx2.Commit(); commitErr != nil {
				mdb.log.Error("Failed to commit transaction: %v", commitErr)
			}
		}
		return correctionErr
	}

	// Update with successful correction
	if err := tx2.Exec("UPDATE transcriptions SET corrected_text = $2, correction_status = 'completed' WHERE file_path = $1", filePath, correctedText); err != nil {
		if rollbackErr := tx2.Rollback(); rollbackErr != nil {
			mdb.log.Error("Failed to rollback transaction: %v", rollbackErr)
		}
		return err
	}

	if err := tx2.Commit(); err != nil {
		return err
	}

	mdb.log.Debug("Correction processed atomically")
	return nil
}

func TestAtomicTransactionSuccess(t *testing.T) {
	mockDB := NewMockDB()

	// Add two successful transactions
	tx1 := &MockTransaction{}
	tx2 := &MockTransaction{}
	mockDB.AddTransaction(tx1)
	mockDB.AddTransaction(tx2)

	// Define a successful correction function
	correctionFunc := func() (string, map[string]interface{}, error) {
		// Simulate some processing time
		time.Sleep(1 * time.Millisecond)
		return "corrected text", map[string]interface{}{"model": "gpt-4"}, nil
	}

	err := mockDB.simulateProcessTranscriptionCorrection(context.Background(), "/test/file.m4b", correctionFunc)

	if err != nil {
		t.Errorf("Expected successful transaction, got error: %v", err)
	}

	// Verify first transaction (status update)
	if tx1.commitCalls != 1 {
		t.Errorf("Expected 1 commit call on first transaction, got %d", tx1.commitCalls)
	}

	if len(tx1.execCalls) != 1 {
		t.Errorf("Expected 1 exec call on first transaction, got %d", len(tx1.execCalls))
	}

	if !tx1.isCommitted {
		t.Error("First transaction should be committed")
	}

	// Verify second transaction (final update)
	if tx2.commitCalls != 1 {
		t.Errorf("Expected 1 commit call on second transaction, got %d", tx2.commitCalls)
	}

	if len(tx2.execCalls) != 1 {
		t.Errorf("Expected 1 exec call on second transaction, got %d", len(tx2.execCalls))
	}

	if !tx2.isCommitted {
		t.Error("Second transaction should be committed")
	}

	// Verify debug message was logged
	if len(mockDB.log.GetDebugMessages()) == 0 {
		t.Error("Expected debug message about atomic processing")
	}
}

func TestAtomicTransactionCorrectionFailure(t *testing.T) {
	mockDB := NewMockDB()

	// Add two transactions
	tx1 := &MockTransaction{}
	tx2 := &MockTransaction{}
	mockDB.AddTransaction(tx1)
	mockDB.AddTransaction(tx2)

	// Define a failing correction function
	correctionError := errors.New("LLM correction failed")
	correctionFunc := func() (string, map[string]interface{}, error) {
		return "", nil, correctionError
	}

	err := mockDB.simulateProcessTranscriptionCorrection(context.Background(), "/test/file.m4b", correctionFunc)

	if err == nil {
		t.Error("Expected error from failing correction function")
	}

	if err != correctionError {
		t.Errorf("Expected correction error, got: %v", err)
	}

	// Verify first transaction committed (status update to in_progress)
	if !tx1.isCommitted {
		t.Error("First transaction should be committed even when correction fails")
	}

	// Verify second transaction was used to record failure
	if tx2.commitCalls != 1 {
		t.Errorf("Expected second transaction to commit failure status, got %d commits", tx2.commitCalls)
	}

	// Verify failure status was set
	if len(tx2.execCalls) != 1 {
		t.Errorf("Expected exec call to set failed status, got %d calls", len(tx2.execCalls))
	}
}

func TestAtomicTransactionFirstCommitFailure(t *testing.T) {
	mockDB := NewMockDB()

	// First transaction fails to commit
	tx1 := &MockTransaction{shouldFailCommit: true}
	mockDB.AddTransaction(tx1)

	correctionFunc := func() (string, map[string]interface{}, error) {
		t.Error("Correction function should not be called if first commit fails")
		return "should not reach", nil, nil
	}

	err := mockDB.simulateProcessTranscriptionCorrection(context.Background(), "/test/file.m4b", correctionFunc)

	if err == nil {
		t.Error("Expected error from failed first commit")
	}

	// Verify transaction was not committed
	if tx1.isCommitted {
		t.Error("Transaction should not be committed when commit fails")
	}
}

func TestAtomicTransactionSecondTransactionFailure(t *testing.T) {
	mockDB := NewMockDB()

	// First transaction succeeds, second fails to begin
	tx1 := &MockTransaction{}
	tx2 := &MockTransaction{shouldFailBegin: true}
	mockDB.AddTransaction(tx1)
	mockDB.AddTransaction(tx2)

	correctionCalled := false
	correctionFunc := func() (string, map[string]interface{}, error) {
		correctionCalled = true
		return "corrected text", nil, nil
	}

	err := mockDB.simulateProcessTranscriptionCorrection(context.Background(), "/test/file.m4b", correctionFunc)

	if err == nil {
		t.Error("Expected error from failed second transaction begin")
	}

	// Verify first transaction still committed
	if !tx1.isCommitted {
		t.Error("First transaction should be committed even if second fails")
	}

	// Verify correction function was called
	if !correctionCalled {
		t.Error("Correction function should be called even if second transaction fails")
	}
}

func TestAtomicTransactionExecFailure(t *testing.T) {
	mockDB := NewMockDB()

	// First transaction exec fails
	tx1 := &MockTransaction{shouldFailExec: true}
	mockDB.AddTransaction(tx1)

	correctionFunc := func() (string, map[string]interface{}, error) {
		t.Error("Correction function should not be called if first exec fails")
		return "", nil, nil
	}

	err := mockDB.simulateProcessTranscriptionCorrection(context.Background(), "/test/file.m4b", correctionFunc)

	if err == nil {
		t.Error("Expected error from failed exec")
	}

	// Verify rollback was called
	if tx1.rollbackCalls != 1 {
		t.Errorf("Expected 1 rollback call, got %d", tx1.rollbackCalls)
	}

	if tx1.isCommitted {
		t.Error("Transaction should not be committed when exec fails")
	}
}

func TestAtomicTransactionContextCancellation(t *testing.T) {
	mockDB := NewMockDB()

	tx1 := &MockTransaction{}
	tx2 := &MockTransaction{}
	mockDB.AddTransaction(tx1)
	mockDB.AddTransaction(tx2)

	// Create a context that will be cancelled
	ctx, cancel := context.WithCancel(context.Background())

	correctionFunc := func() (string, map[string]interface{}, error) {
		// Cancel the context during correction
		cancel()
		// Check if context is cancelled
		if ctx.Err() != nil {
			return "", nil, ctx.Err()
		}
		return "corrected", nil, nil
	}

	err := mockDB.simulateProcessTranscriptionCorrection(ctx, "/test/file.m4b", correctionFunc)

	if err == nil {
		t.Error("Expected error from context cancellation")
	}

	if err != context.Canceled {
		t.Errorf("Expected context cancelled error, got: %v", err)
	}

	// First transaction should still be committed (status update happened before cancellation)
	if !tx1.isCommitted {
		t.Error("First transaction should be committed even with context cancellation")
	}

	// Second transaction should record the failure
	if !tx2.isCommitted {
		t.Error("Second transaction should commit the failure status")
	}
}

func TestAtomicTransactionRaceCondition(t *testing.T) {
	// This test simulates potential race conditions in atomic transaction handling
	mockDB := NewMockDB()

	// Add multiple transaction pairs for concurrent operations
	for i := 0; i < 4; i++ {
		tx1 := &MockTransaction{}
		tx2 := &MockTransaction{}
		mockDB.AddTransaction(tx1)
		mockDB.AddTransaction(tx2)
	}

	// Simulate concurrent corrections
	done := make(chan error, 2)

	correctionFunc := func(id string) func() (string, map[string]interface{}, error) {
		return func() (string, map[string]interface{}, error) {
			// Simulate processing time
			time.Sleep(1 * time.Millisecond)
			return "corrected " + id, map[string]interface{}{"id": id}, nil
		}
	}

	go func() {
		err := mockDB.simulateProcessTranscriptionCorrection(context.Background(), "/test/file1.m4b", correctionFunc("1"))
		done <- err
	}()

	go func() {
		err := mockDB.simulateProcessTranscriptionCorrection(context.Background(), "/test/file2.m4b", correctionFunc("2"))
		done <- err
	}()

	// Wait for both to complete
	for i := 0; i < 2; i++ {
		err := <-done
		if err != nil {
			t.Errorf("Concurrent correction %d failed: %v", i+1, err)
		}
	}

	// Verify all transactions were properly committed
	expectedCommits := 4 // 2 files * 2 transactions each
	actualCommits := 0
	for _, tx := range mockDB.transactions[:4] { // First 4 transactions
		actualCommits += tx.commitCalls
	}

	if actualCommits != expectedCommits {
		t.Errorf("Expected %d commits from concurrent operations, got %d", expectedCommits, actualCommits)
	}
}

func TestAtomicTransactionPartialFailureRecovery(t *testing.T) {
	// Test the resilience when some operations fail but others succeed
	mockDB := NewMockDB()

	// First attempt: status update succeeds, final update fails
	tx1 := &MockTransaction{}
	tx2 := &MockTransaction{shouldFailExec: true}
	mockDB.AddTransaction(tx1)
	mockDB.AddTransaction(tx2)

	correctionFunc := func() (string, map[string]interface{}, error) {
		return "corrected text", nil, nil
	}

	err := mockDB.simulateProcessTranscriptionCorrection(context.Background(), "/test/file.m4b", correctionFunc)

	if err == nil {
		t.Error("Expected error from failed final update")
	}

	// Verify the first transaction committed (status is 'in_progress')
	if !tx1.isCommitted {
		t.Error("Status update transaction should have committed")
	}

	// Verify the second transaction was rolled back
	if tx2.isCommitted {
		t.Error("Failed transaction should not be committed")
	}

	if tx2.rollbackCalls != 1 {
		t.Errorf("Expected 1 rollback call, got %d", tx2.rollbackCalls)
	}

	// In a real scenario, this would leave the file in 'in_progress' status,
	// which could be detected and handled by monitoring systems
}

func TestAtomicTransactionIdempotency(t *testing.T) {
	// Test that the same correction can be attempted multiple times safely
	mockDB := NewMockDB()

	// Add transactions for multiple attempts
	for i := 0; i < 4; i++ {
		tx1 := &MockTransaction{}
		tx2 := &MockTransaction{}
		mockDB.AddTransaction(tx1)
		mockDB.AddTransaction(tx2)
	}

	filePath := "/test/idempotent.m4b"
	correctionFunc := func() (string, map[string]interface{}, error) {
		return "consistently corrected text", map[string]interface{}{"attempt": "any"}, nil
	}

	// First attempt should succeed
	err := mockDB.simulateProcessTranscriptionCorrection(context.Background(), filePath, correctionFunc)
	if err != nil {
		t.Errorf("First attempt should succeed: %v", err)
	}

	// Second attempt should also succeed (simulating retry logic)
	err = mockDB.simulateProcessTranscriptionCorrection(context.Background(), filePath, correctionFunc)
	if err != nil {
		t.Errorf("Second attempt should succeed: %v", err)
	}

	// Both attempts should have committed their transactions
	if mockDB.transactions[0].commitCalls != 1 || mockDB.transactions[1].commitCalls != 1 {
		t.Error("First attempt transactions should be committed")
	}

	if mockDB.transactions[2].commitCalls != 1 || mockDB.transactions[3].commitCalls != 1 {
		t.Error("Second attempt transactions should be committed")
	}
}
