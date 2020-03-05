package fluxmonitor_test

import (
	"fmt"
	"math"
	"math/big"
	"net/url"
	"reflect"
	"testing"
	"time"

	"chainlink/core/cmd"
	"chainlink/core/eth"
	"chainlink/core/eth/contracts"
	"chainlink/core/internal/cltest"
	"chainlink/core/internal/mocks"
	"chainlink/core/services/fluxmonitor"
	"chainlink/core/store"
	"chainlink/core/store/models"
	"chainlink/core/utils"

	"github.com/ethereum/go-ethereum/common"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
)

var (
	updateAnswerHash     = utils.MustHash("updateAnswer(uint256,int256)")
	updateAnswerSelector = updateAnswerHash[:4]
)

type successFetcher decimal.Decimal

func (f *successFetcher) Fetch() (decimal.Decimal, error) {
	return decimal.Decimal(*f), nil
}

//func fakeSubscription() *mocks.LogSubscription {
//    sub := new(mocks.LogSubscription)
//    sub.On("Unsubscribe").Return()
//    sub.On("Logs").Return(make(<-chan contracts.MaybeDecodedLog))
//    return sub
//}

func TestConcreteFluxMonitor_AddJobRemoveJobHappy(t *testing.T) {
	store, cleanup := cltest.NewStore(t)
	defer cleanup()

	job := cltest.NewJobWithFluxMonitorInitiator()
	runManager := new(mocks.RunManager)
	started := make(chan struct{}, 1)

	dc := new(mocks.DeviationChecker)
	dc.On("Start", mock.Anything, mock.Anything).Return(nil).Run(func(mock.Arguments) {
		started <- struct{}{}
	})

	checkerFactory := new(mocks.DeviationCheckerFactory)
	checkerFactory.On("New", job.Initiators[0], runManager, store.ORM).Return(dc, nil)
	fm := fluxmonitor.New(store, runManager)
	fluxmonitor.ExportedSetCheckerFactory(fm, checkerFactory)
	require.NoError(t, fm.Start())
	defer fm.Stop()

	// Add Job
	require.NoError(t, fm.AddJob(job))

	cltest.CallbackOrTimeout(t, "deviation checker started", func() {
		<-started
	})
	checkerFactory.AssertExpectations(t)
	dc.AssertExpectations(t)

	// Remove Job
	removed := make(chan struct{})
	dc.On("Stop").Return().Run(func(mock.Arguments) {
		removed <- struct{}{}
	})
	fm.RemoveJob(job.ID)
	cltest.CallbackOrTimeout(t, "deviation checker stopped", func() {
		<-removed
	})
	dc.AssertExpectations(t)
}

func TestConcreteFluxMonitor_AddJobNonFluxMonitor(t *testing.T) {
	store, cleanup := cltest.NewStore(t)
	defer cleanup()

	job := cltest.NewJobWithRunLogInitiator()
	runManager := new(mocks.RunManager)
	checkerFactory := new(mocks.DeviationCheckerFactory)
	fm := fluxmonitor.New(store, runManager)
	fluxmonitor.ExportedSetCheckerFactory(fm, checkerFactory)
	require.NoError(t, fm.Start())
	defer fm.Stop()

	require.NoError(t, fm.AddJob(job))
}

//func TestConcreteFluxMonitor_AddJobDisconnected(t *testing.T) {
//    store, cleanup := cltest.NewStore(t)
//    defer cleanup()
//
//    job := cltest.NewJobWithFluxMonitorInitiator()
//    runManager := new(mocks.RunManager)
//    checkerFactory := new(mocks.DeviationCheckerFactory)
//    dc := new(mocks.DeviationChecker)
//    checkerFactory.On("New", job.Initiators[0], runManager, store.ORM).Return(dc, nil)
//    fm := fluxmonitor.New(store, runManager)
//    fluxmonitor.ExportedSetCheckerFactory(fm, checkerFactory)
//    require.NoError(t, fm.Start())
//    defer fm.Stop()
//
//    require.NoError(t, fm.AddJob(job))
//}
//
//func TestConcreteFluxMonitor_ConnectStartsExistingJobs(t *testing.T) {
//    store, cleanup := cltest.NewStore(t)
//    defer cleanup()
//
//    runManager := new(mocks.RunManager)
//    started := make(chan struct{})
//
//    dc := new(mocks.DeviationChecker)
//    dc.On("Start", mock.Anything, mock.Anything).Return(nil).Run(func(mock.Arguments) {
//        started <- struct{}{}
//    })
//
//    checkerFactory := new(mocks.DeviationCheckerFactory)
//
//    for i := 0; i < 3; i++ {
//        job := cltest.NewJobWithFluxMonitorInitiator()
//        require.NoError(t, store.CreateJob(&job))
//        job, err := store.FindJob(job.ID)
//        require.NoError(t, err)
//        checkerFactory.On("New", job.Initiators[0], runManager, store.ORM).Return(dc, nil)
//    }
//
//    fm := fluxmonitor.New(store, runManager)
//    fluxmonitor.ExportedSetCheckerFactory(fm, checkerFactory)
//    err := fm.Start()
//    require.NoError(t, err)
//    defer fm.Stop()
//
//    require.NoError(t, fm.Connect(nil))
//    cltest.CallbackOrTimeout(t, "deviation checker started", func() {
//        <-started
//    })
//    checkerFactory.AssertExpectations(t)
//    dc.AssertExpectations(t)
//}

func TestConcreteFluxMonitor_StopWithoutStart(t *testing.T) {
	store, cleanup := cltest.NewStore(t)
	defer cleanup()

	runManager := new(mocks.RunManager)

	fm := fluxmonitor.New(store, runManager)
	fm.Stop()
}

func TestPollingDeviationChecker_PollIfEligible(t *testing.T) {
	tests := []struct {
		name                      string
		eligible                  bool
		connected                 bool
		threshold                 float64
		latestAnswer              int64
		polledAnswer              int64
		expectedToFetchRoundState bool
		expectedToPoll            bool
		expectedToSubmit          bool
	}{
		{"eligible, connected, threshold > 0, answers deviate", true, true, 0.1, 1, 100, true, true, true},
		{"eligible, connected, threshold > 0, answers do not deviate", true, true, 0.1, 100, 100, true, true, false},
		{"eligible, connected, threshold == 0, answers deviate", true, true, 0, 1, 100, true, true, true},
		{"eligible, connected, threshold == 0, answers do not deviate", true, true, 0, 1, 100, true, true, true},

		{"eligible, disconnected, threshold > 0, answers deviate", true, false, 0.1, 1, 100, false, false, false},
		{"eligible, disconnected, threshold > 0, answers do not deviate", true, false, 0.1, 100, 100, false, false, false},
		{"eligible, disconnected, threshold == 0, answers deviate", true, false, 0, 1, 100, false, false, false},
		{"eligible, disconnected, threshold == 0, answers do not deviate", true, false, 0, 1, 100, false, false, false},

		{"ineligible, connected, threshold > 0, answers deviate", false, true, 0.1, 1, 100, true, false, false},
		{"ineligible, connected, threshold > 0, answers do not deviate", false, true, 0.1, 100, 100, true, false, false},
		{"ineligible, connected, threshold == 0, answers deviate", false, true, 0, 1, 100, true, false, false},
		{"ineligible, connected, threshold == 0, answers do not deviate", false, true, 0, 1, 100, true, false, false},

		{"ineligible, disconnected, threshold > 0, answers deviate", false, false, 0.1, 1, 100, false, false, false},
		{"ineligible, disconnected, threshold > 0, answers do not deviate", false, false, 0.1, 100, 100, false, false, false},
		{"ineligible, disconnected, threshold == 0, answers deviate", false, false, 0, 1, 100, false, false, false},
		{"ineligible, disconnected, threshold == 0, answers do not deviate", false, false, 0, 1, 100, false, false, false},
	}

	store, cleanup := cltest.NewStore(t)
	defer cleanup()

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			rm := new(mocks.RunManager)
			fetcher := new(mocks.Fetcher)
			fluxAggregator := new(mocks.FluxAggregator)

			job := cltest.NewJobWithFluxMonitorInitiator()
			initr := job.Initiators[0]
			initr.ID = 1

			const reportableRoundID = 2
			latestAnswerNoPrecision := test.latestAnswer * int64(math.Pow10(int(initr.InitiatorParams.Precision)))

			if test.expectedToFetchRoundState {
				roundState := contracts.FluxAggregatorRoundState{
					ReportableRoundID: big.NewInt(reportableRoundID),
					EligibleToSubmit:  test.eligible,
					LatestAnswer:      big.NewInt(latestAnswerNoPrecision),
				}
				fluxAggregator.On("RoundState").Return(roundState, nil).
					Once()
			}

			if test.expectedToPoll {
				fetcher.On("Fetch").Return(decimal.NewFromInt(test.polledAnswer), nil)
			}

			if test.expectedToSubmit {
				run := cltest.NewJobRun(job)
				data, err := models.ParseJSON([]byte(fmt.Sprintf(`{
					"result": "%d",
					"address": "%s",
					"functionSelector": "0x%x",
					"dataPrefix": "0x000000000000000000000000000000000000000000000000000000000000000%d"
				}`, test.polledAnswer, initr.InitiatorParams.Address.Hex(), updateAnswerSelector, reportableRoundID)))
				require.NoError(t, err)

				rm.On("Create", job.ID, &initr, mock.Anything, mock.MatchedBy(func(runRequest *models.RunRequest) bool {
					return reflect.DeepEqual(runRequest.RequestParams.Result.Value(), data.Result.Value())
				})).Return(&run, nil)

				fluxAggregator.On("GetMethodID", "updateAnswer").Return(updateAnswerSelector, nil)
			}

			checker, err := fluxmonitor.NewPollingDeviationChecker(store, fluxAggregator, initr, rm, fetcher, time.Second)
			require.NoError(t, err)

			if test.connected {
				checker.OnConnect()
			}

			checker.ExportedPollIfEligible(test.threshold)

			fluxAggregator.AssertExpectations(t)
			fetcher.AssertExpectations(t)
			rm.AssertExpectations(t)
		})
	}
}

func TestPollingDeviationChecker_TriggerIdleTimeThreshold(t *testing.T) {
	store, cleanup := cltest.NewStore(t)
	defer cleanup()

	tests := []struct {
		name             string
		idleThreshold    time.Duration
		expectedToSubmit bool
	}{
		{"no idleThreshold", 0, false},
		{"idleThreshold > 0", 10 * time.Millisecond, true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			runManager := new(mocks.RunManager)
			fluxAggregator := new(mocks.FluxAggregator)
			fetcher := new(mocks.Fetcher)

			job := cltest.NewJobWithFluxMonitorInitiator()
			initr := job.Initiators[0]
			initr.ID = 1
			initr.PollingInterval = models.Duration(math.MaxInt64)
			initr.IdleThreshold = models.Duration(test.idleThreshold)
			jobRun := cltest.NewJobRun(job)

			const fetchedAnswer = 100
			answerBigInt := big.NewInt(fetchedAnswer * int64(math.Pow10(int(initr.InitiatorParams.Precision))))

			fluxAggregator.On("SubscribeToLogs", mock.Anything).Return(eth.UnsubscribeFunc(func() {}), nil)

			roundState1 := contracts.FluxAggregatorRoundState{ReportableRoundID: big.NewInt(1), EligibleToSubmit: true, LatestAnswer: answerBigInt}
			roundState2 := contracts.FluxAggregatorRoundState{ReportableRoundID: big.NewInt(2), EligibleToSubmit: true, LatestAnswer: answerBigInt}
			roundState3 := contracts.FluxAggregatorRoundState{ReportableRoundID: big.NewInt(3), EligibleToSubmit: true, LatestAnswer: answerBigInt}
			fluxAggregator.On("RoundState").Return(roundState1, nil).Once()
			fetcher.On("Fetch").Return(decimal.NewFromInt(fetchedAnswer), nil).Once()

			jobRunCreated := make(chan struct{}, 3)

			if test.expectedToSubmit {
				fetcher.On("Fetch").Return(decimal.NewFromInt(fetchedAnswer), nil)

				fluxAggregator.On("GetMethodID", "updateAnswer").Return(updateAnswerSelector, nil).Times(3)
				fluxAggregator.On("RoundState").Return(roundState1, nil).Once()
				fluxAggregator.On("RoundState").Return(roundState2, nil).Once()
				fluxAggregator.On("RoundState").Return(roundState3, nil).Once()

				runManager.On("Create", job.ID, mock.Anything, mock.Anything, mock.Anything, mock.Anything).
					Return(&jobRun, nil).
					Run(func(args mock.Arguments) {
						jobRunCreated <- struct{}{}
					})
			}

			deviationChecker, err := fluxmonitor.NewPollingDeviationChecker(
				store,
				fluxAggregator,
				initr,
				runManager,
				fetcher,
				time.Duration(math.MaxInt64),
			)
			require.NoError(t, err)

			deviationChecker.Start()
			require.Len(t, jobRunCreated, 0, "no Job Runs created")

			deviationChecker.OnConnect()

			if test.expectedToSubmit {
				require.Eventually(t, func() bool { return len(jobRunCreated) == 1 }, time.Second, 10*time.Millisecond)
				deviationChecker.HandleLog(&contracts.LogAnswerUpdated{RoundId: roundState1.ReportableRoundID}, nil)
				require.Eventually(t, func() bool { return len(jobRunCreated) == 2 }, time.Second, 10*time.Millisecond)
				deviationChecker.HandleLog(&contracts.LogAnswerUpdated{RoundId: roundState2.ReportableRoundID}, nil)
				require.Eventually(t, func() bool { return len(jobRunCreated) == 3 }, time.Second, 10*time.Millisecond)
			}

			deviationChecker.Stop()

			if !test.expectedToSubmit {
				require.Len(t, jobRunCreated, 0)
			}

			fetcher.AssertExpectations(t)
			runManager.AssertExpectations(t)
			fluxAggregator.AssertExpectations(t)
		})
	}
}

//func TestPollingDeviationChecker_StartError(t *testing.T) {
//    store, cleanup := cltest.NewStore(t)
//    defer cleanup()
//
//    rm := new(mocks.RunManager)
//    job := cltest.NewJobWithFluxMonitorInitiator()
//    initr := job.Initiators[0]
//    initr.ID = 1
//
//    fluxAggregator := new(mocks.FluxAggregator)
//    fluxAggregator.On("LatestAnswer", initr.InitiatorParams.Precision).
//        Return(decimal.NewFromInt(0), errors.New("deliberate test error"))
//
//    checker, err := fluxmonitor.NewPollingDeviationChecker(store, fluxAggregator, initr, rm, nil, time.Second)
//    require.NoError(t, err)
//    require.Error(t, checker.Start(context.Background()))
//}
//
//func TestPollingDeviationChecker_StartStop(t *testing.T) {
//    store, cleanup := cltest.NewStore(t)
//    defer cleanup()
//
//    // Prepare initialization to 100, which matches external adapter, so no deviation
//    job := cltest.NewJobWithFluxMonitorInitiator()
//    initr := job.Initiators[0]
//    initr.ID = 1
//
//    const fetchedAnswer = 100
//
//    answerBigInt := big.NewInt(fetchedAnswer * int64(math.Pow10(int(initr.InitiatorParams.Precision))))
//
//    roundState := contracts.FluxAggregatorRoundState{
//        ReportableRoundID: big.NewInt(1),
//        EligibleToSubmit:  true,
//        LatestAnswer:      answerBigInt,
//    }
//
//    fluxAggregator := new(mocks.FluxAggregator)
//    fluxAggregator.On("RoundState").Return(roundState, nil)
//    fluxAggregator.On("SubscribeToLogs", mock.Anything).Return(eth.UnsubscribeFunc(func() {}), nil)
//    fluxAggregator.On("GetMethodID", "updateAnswer").Return(updateAnswerSelector, nil)
//
//    rm := new(mocks.RunManager)
//    fetcher := new(mocks.Fetcher)
//    checker, err := fluxmonitor.NewPollingDeviationChecker(store, fluxAggregator, initr, rm, fetcher, time.Millisecond)
//    require.NoError(t, err)
//
//    // Set up fetcher to mark when polled
//    started := make(chan struct{})
//    fetcher.On("Fetch").Return(decimal.NewFromInt(2*fetchedAnswer), nil).Run(func(mock.Arguments) {
//        started <- struct{}{}
//    })
//
//    // Start() with no delay to speed up test and polling.
//    done := make(chan struct{})
//    go func() {
//        checker.Start() // Start() polling
//        checker.OnConnect()
//        done <- struct{}{}
//    }()
//
//    cltest.CallbackOrTimeout(t, "Start() starts", func() {
//        <-started
//    })
//
//    checker.Stop()
//    cltest.CallbackOrTimeout(t, "Stop() unblocks Start()", func() {
//        <-done
//    })
//
//    rm.AssertExpectations(t)
//    fetcher.AssertExpectations(t)
//    fluxAggregator.AssertExpectations(t)
//}

//func TestPollingDeviationChecker_NoDeviation_CanBeCanceled(t *testing.T) {
//    store, cleanup := cltest.NewStore(t)
//    defer cleanup()
//
//    const fetchedAnswer = 100
//
//    // Set up fetcher to mark when polled
//    fetcher := new(mocks.Fetcher)
//    polled := make(chan struct{})
//    fetcher.On("Fetch").Return(decimal.NewFromInt(fetchedAnswer), nil).Run(func(mock.Arguments) {
//        polled <- struct{}{}
//    })
//
//    // Prepare initialization to 100, which matches external adapter, so no deviation
//    job := cltest.NewJobWithFluxMonitorInitiator()
//    initr := job.Initiators[0]
//    initr.ID = 1
//
//    answerBigInt := big.NewInt(fetchedAnswer * int64(math.Pow10(int(initr.InitiatorParams.Precision))))
//
//    roundState := contracts.FluxAggregatorRoundState{
//        RoundID:                big.NewInt(1),
//        StartedAt:              big.NewInt(time.Now().UTC().Unix() - 100),
//        UpdatedAt:              big.NewInt(0),
//        Answer:                 answerBigInt,
//        PrevRoundAnswer:        answerBigInt,
//        RoundTimeout:           big.NewInt(120),
//        PaymentAmount:          big.NewInt(1),
//        MinAnswerCount:         big.NewInt(1),
//        MaxAnswerCount:         big.NewInt(1),
//        RestartDelay:           big.NewInt(0),
//        OracleLastStartedRound: big.NewInt(1),
//        AvailableFunds:         big.NewInt(10),
//    }
//
//    fluxAggregator := new(mocks.FluxAggregator)
//    fluxAggregator.On("CurrentRoundState", nodeAddr).Return(roundState, nil)
//    fluxAggregator.On("SubscribeToLogs", mock.Anything).Return(eth.UnsubscribeFunc(func() {}), nil)
//
//    // Start() with no delay to speed up test and polling.
//    rm := new(mocks.RunManager) // No mocks assert no runs are created
//    checker, err := fluxmonitor.NewPollingDeviationChecker(store, fluxAggregator, initr, rm, fetcher, time.Millisecond)
//    require.NoError(t, err)
//
//    ctx, cancel := context.WithCancel(context.Background())
//    done := make(chan struct{})
//    go func() {
//        checker.Start(ctx) // Start() polling until cancel()
//        done <- struct{}{}
//    }()
//
//    // Check if Polled
//    cltest.CallbackOrTimeout(t, "start repeatedly polls external adapter", func() {
//        <-polled // launched at the beginning of Start
//        <-polled // launched after time.After
//    })
//
//    // Cancel parent context and ensure Start() stops.
//    cancel()
//    cltest.CallbackOrTimeout(t, "Start() unblocks and is done", func() {
//        <-done
//    })
//
//    rm.AssertExpectations(t)
//    fetcher.AssertExpectations(t)
//    fluxAggregator.AssertExpectations(t)
//}

func TestPollingDeviationChecker_RespondToNewRound_Ignore(t *testing.T) {
	store, cleanup := cltest.NewStore(t)
	defer cleanup()

	const (
		currentRound = 5
		answer       = 100
	)

	job := cltest.NewJobWithFluxMonitorInitiator()
	initr := job.Initiators[0]
	initr.ID = 1
	initr.InitiatorParams.IdleThreshold = 0

	rm := new(mocks.RunManager)
	fetcher := new(mocks.Fetcher)
	fluxAggregator := new(mocks.FluxAggregator)

	answerBigInt := big.NewInt(answer * int64(math.Pow10(int(initr.InitiatorParams.Precision))))

	roundState := contracts.FluxAggregatorRoundState{
		ReportableRoundID: big.NewInt(currentRound),
		EligibleToSubmit:  true,
		LatestAnswer:      answerBigInt,
	}

	fluxAggregator.On("SubscribeToLogs", mock.Anything).Return(eth.UnsubscribeFunc(func() {}), nil)
	fluxAggregator.On("RoundState").Return(roundState, nil)

	fetcher.On("Fetch").Return(decimal.NewFromInt(answer), nil).Once()

	checker, err := fluxmonitor.NewPollingDeviationChecker(store, fluxAggregator, initr, rm, fetcher, time.Hour)
	require.NoError(t, err)

	checker.Start()
	checker.OnConnect()

	// Send rounds less than or equal to current, sequentially
	tests := []struct {
		name  string
		round int64
	}{
		{"less than", currentRound - 1},
		{"equal", currentRound},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			checker.HandleLog(&contracts.LogNewRound{RoundId: big.NewInt(test.round)}, nil)

			fluxAggregator.AssertExpectations(t)
			fetcher.AssertExpectations(t)
			rm.AssertNotCalled(t, "Create", mock.Anything, mock.Anything, mock.Anything, mock.Anything)
		})
	}
}

func TestPollingDeviationChecker_RespondToNewRound_Respond(t *testing.T) {
	store, cleanup := cltest.NewStore(t)
	defer cleanup()

	const (
		currentRound = 5
		answer       = 100
	)

	// Prepare on-chain initialization to 100, which matches external adapter,
	// so no deviation
	job := cltest.NewJobWithFluxMonitorInitiator()
	initr := job.Initiators[0]
	initr.ID = 1

	answerBigInt := big.NewInt(answer * int64(math.Pow10(int(initr.InitiatorParams.Precision))))

	roundState := contracts.FluxAggregatorRoundState{
		ReportableRoundID: big.NewInt(currentRound),
		LatestAnswer:      answerBigInt,
		EligibleToSubmit:  true,
	}

	rm := new(mocks.RunManager)
	fetcher := new(mocks.Fetcher)
	fluxAggregator := new(mocks.FluxAggregator)

	fluxAggregator.On("SubscribeToLogs", mock.Anything).Return(eth.UnsubscribeFunc(func() {}), nil)
	fluxAggregator.On("CurrentRoundState", nodeAddr).Return(roundState, nil)
	fluxAggregator.On("RoundState", nodeAddr, mock.Anything).Return(roundState, nil)

	fluxAggregator.On("GetMethodID", "updateAnswer").Return(updateAnswerSelector, nil)

	// Initialize
	checker, err := fluxmonitor.NewPollingDeviationChecker(store, fluxAggregator, initr, rm, fetcher, time.Hour)
	require.NoError(t, err)
	err = checker.ExportedFetchAggregatorData(nil)
	require.NoError(t, err)

	// Send log greater than current
	data, err := models.ParseJSON([]byte(fmt.Sprintf(`{
            "result": "100",
            "address": "%s",
            "functionSelector": "0xe6330cf7",
            "dataPrefix": "0x%0x"
    }`, initr.InitiatorParams.Address.Hex(), utils.EVMWordUint64(currentRound+1)))) // dataPrefix has currentRound + 1
	require.NoError(t, err)

	// Set up fetcher for 100; even if within deviation, forces the creation of run.
	fetcher.On("Fetch").Return(decimal.NewFromInt(answer), nil)

	rm.On("Create", mock.Anything, mock.Anything, mock.Anything, mock.MatchedBy(func(runRequest *models.RunRequest) bool {
		return runRequest.RequestParams == data
	})).Return(nil, nil) // only round 6 triggers run.

	_, err = checker.ExportedRespondToLog(&contracts.LogNewRound{RoundId: big.NewInt(currentRound + 1)})
	require.NoError(t, err)

	fluxAggregator.AssertExpectations(t)
	fetcher.AssertExpectations(t)
	rm.AssertExpectations(t)
}

func TestOutsideDeviation(t *testing.T) {
	tests := []struct {
		name                string
		curPrice, nextPrice decimal.Decimal
		threshold           float64 // in percentage
		expectation         bool
	}{
		{"0 current price", decimal.NewFromInt(0), decimal.NewFromInt(100), 2, true},
		{"inside deviation", decimal.NewFromInt(100), decimal.NewFromInt(101), 2, false},
		{"equal to deviation", decimal.NewFromInt(100), decimal.NewFromInt(102), 2, true},
		{"outside deviation", decimal.NewFromInt(100), decimal.NewFromInt(103), 2, true},
		{"outside deviation zero", decimal.NewFromInt(100), decimal.NewFromInt(0), 2, true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			actual := fluxmonitor.OutsideDeviation(test.curPrice, test.nextPrice, test.threshold)
			assert.Equal(t, test.expectation, actual)
		})
	}
}

func TestExtractFeedURLs(t *testing.T) {
	store, cleanup := cltest.NewStore(t)
	defer cleanup()

	bridge := &models.BridgeType{
		Name: models.MustNewTaskType("testbridge"),
		URL:  cltest.WebURL(t, "https://testing.com/bridges"),
	}
	require.NoError(t, store.CreateBridgeType(bridge))

	tests := []struct {
		name        string
		in          string
		expectation []string
	}{
		{
			"single",
			`["https://lambda.staging.devnet.tools/bnc/call"]`,
			[]string{"https://lambda.staging.devnet.tools/bnc/call"},
		},
		{
			"double",
			`["https://lambda.staging.devnet.tools/bnc/call", "https://lambda.staging.devnet.tools/cc/call"]`,
			[]string{"https://lambda.staging.devnet.tools/bnc/call", "https://lambda.staging.devnet.tools/cc/call"},
		},
		{
			"bridge",
			`[{"bridge":"testbridge"}]`,
			[]string{"https://testing.com/bridges"},
		},
		{
			"mixed",
			`["https://lambda.staging.devnet.tools/bnc/call", {"bridge": "testbridge"}]`,
			[]string{"https://lambda.staging.devnet.tools/bnc/call", "https://testing.com/bridges"},
		},
		{
			"empty",
			`[]`,
			[]string{},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			initiatorParams := models.InitiatorParams{
				Feeds: cltest.JSONFromString(t, test.in),
			}
			var expectation []*url.URL
			for _, urlString := range test.expectation {
				expectation = append(expectation, cltest.MustParseURL(urlString))
			}
			val, err := fluxmonitor.ExtractFeedURLs(initiatorParams.Feeds, store.ORM)
			require.NoError(t, err)
			assert.Equal(t, val, expectation)
		})
	}
}
