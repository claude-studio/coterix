package ui

import (
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
	"github.com/ridenow/coterix/internal/pipeline"
	"github.com/ridenow/coterix/internal/state"
)

func fixedClock(at time.Time) func() time.Time {
	return func() time.Time { return at }
}

func TestStepperDerivesStateOnlyStages(t *testing.T) {
	base := time.Date(2026, 7, 24, 21, 0, 0, 0, time.UTC)

	cases := []struct {
		name      string
		phase     state.Phase
		step      string
		pending   *state.PendingAction
		lastStep  string
		expect    map[pipelineStage]stepperRowState
		hasStatus bool
	}{
		{
			name:      "planning with plan step",
			phase:     state.PhasePlanning,
			step:      pipeline.StepPlan,
			hasStatus: true,
			expect: map[pipelineStage]stepperRowState{
				stagePlan:   stepperActive,
				stageReview: stepperPending,
			},
		},
		{
			name:      "planning with plan review step",
			phase:     state.PhasePlanning,
			step:      pipeline.StepPlanReview,
			hasStatus: true,
			expect: map[pipelineStage]stepperRowState{
				stagePlan:   stepperDone,
				stageReview: stepperActive,
			},
		},
		{
			name:      "awaiting approval",
			phase:     state.PhaseAwaitingApproval,
			hasStatus: true,
			expect: map[pipelineStage]stepperRowState{
				stagePlan:      stepperDone,
				stageReview:    stepperDone,
				stageApprove:   stepperActive,
				stageImplement: stepperPending,
			},
		},
		{
			name:      "implementing with gate step",
			phase:     state.PhaseImplementing,
			step:      pipeline.StepGate,
			hasStatus: true,
			expect: map[pipelineStage]stepperRowState{
				stageApprove:   stepperDone,
				stageImplement: stepperDone,
				stageVerify:    stepperActive,
			},
		},
		{
			name:      "implementing with fix step",
			phase:     state.PhaseImplementing,
			step:      pipeline.StepFix,
			hasStatus: true,
			expect: map[pipelineStage]stepperRowState{
				stageImplement: stepperActive,
				stageVerify:    stepperPending,
			},
		},
		{
			name:      "paused keeps the observed stage as waiting",
			phase:     state.PhasePausedForInput,
			lastStep:  pipeline.StepImplementation,
			hasStatus: true,
			pending: &state.PendingAction{
				Kind:        state.PendingAuth,
				ResumePhase: state.PhaseImplementing,
			},
			expect: map[pipelineStage]stepperRowState{
				stageImplement: stepperWaiting,
				stageVerify:    stepperPending,
			},
		},
		{
			name:      "paused without observation maps task cap",
			phase:     state.PhasePausedForInput,
			hasStatus: true,
			pending: &state.PendingAction{
				Kind:        state.PendingTaskCap,
				ResumePhase: state.PhaseImplementing,
			},
			expect: map[pipelineStage]stepperRowState{
				stageImplement: stepperWaiting,
			},
		},
		{
			name:      "done marks every stage",
			phase:     state.PhaseDone,
			hasStatus: true,
			expect: map[pipelineStage]stepperRowState{
				stagePlan:      stepperDone,
				stageReview:    stepperDone,
				stageApprove:   stepperDone,
				stageImplement: stepperDone,
				stageVerify:    stepperDone,
			},
		},
		{
			name:      "failed marks the observed stage",
			phase:     state.PhaseFailed,
			lastStep:  pipeline.StepImplementationReview,
			hasStatus: true,
			expect: map[pipelineStage]stepperRowState{
				stageImplement: stepperDone,
				stageVerify:    stepperFailed,
			},
		},
		{
			name: "no status starts at plan",
			expect: map[pipelineStage]stepperRowState{
				stagePlan:   stepperActive,
				stageReview: stepperPending,
			},
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			current := testModel(t, &fakeUIControl{})
			current.now = fixedClock(base)
			current.hasStatus = testCase.hasStatus
			current.status.Phase = testCase.phase
			current.status.PendingAction = testCase.pending
			current.activeStep = testCase.step
			if testCase.lastStep != "" {
				current.stages.stepStarted(testCase.lastStep, base)
				current.stages.stepFinished(
					testCase.lastStep,
					base.Add(time.Second),
				)
			}

			rows := deriveStepper(current)
			for stage, expected := range testCase.expect {
				if rows[stage].State != expected {
					t.Fatalf(
						"stage %s state=%d, want %d (rows=%#v)",
						stageLabels[stage],
						rows[stage].State,
						expected,
						rows,
					)
				}
			}
		})
	}
}

func TestStageClockAccumulatesDeterministicDurations(t *testing.T) {
	base := time.Date(2026, 7, 24, 21, 0, 0, 0, time.UTC)
	clock := stageClock{}

	clock.stepStarted(pipeline.StepPlan, base)
	clock.stepFinished(pipeline.StepPlan, base.Add(1200*time.Millisecond))
	clock.stepStarted(pipeline.StepPlanReview, base.Add(2*time.Second))
	clock.stepFinished(
		pipeline.StepPlanReview,
		base.Add(2*time.Second+800*time.Millisecond),
	)
	clock.observePhase(state.PhaseAwaitingApproval, base.Add(3*time.Second))
	clock.observePhase(state.PhaseImplementing, base.Add(5*time.Second))
	clock.stepStarted(pipeline.StepImplementation, base.Add(5*time.Second))

	now := base.Add(8 * time.Second)
	if got := clock.elapsedAt(stagePlan, now); got != 1200*time.Millisecond {
		t.Fatalf("plan elapsed=%s", got)
	}
	if got := clock.elapsedAt(stageReview, now); got != 800*time.Millisecond {
		t.Fatalf("review elapsed=%s", got)
	}
	if got := clock.elapsedAt(stageApprove, now); got != 2*time.Second {
		t.Fatalf("approve elapsed=%s", got)
	}
	// The active stage keeps counting against the injected now.
	if got := clock.elapsedAt(stageImplement, now); got != 3*time.Second {
		t.Fatalf("implement elapsed=%s", got)
	}
	// Fix attempts accumulate into the same stage.
	clock.stepFinished(pipeline.StepImplementation, base.Add(9*time.Second))
	clock.stepStarted(pipeline.StepFix, base.Add(10*time.Second))
	clock.stepFinished(pipeline.StepFix, base.Add(11*time.Second))
	if got := clock.elapsedAt(
		stageImplement,
		base.Add(20*time.Second),
	); got != 5*time.Second {
		t.Fatalf("implement accumulated elapsed=%s", got)
	}
	// Unknown steps never move the clock.
	clock.stepStarted("unknown_step", base.Add(30*time.Second))
	if clock.hasActive {
		t.Fatal("unknown step activated the stage clock")
	}
}

func TestFormatStageDurationBuckets(t *testing.T) {
	cases := map[time.Duration]string{
		0:                                    "",
		1200 * time.Millisecond:              "1.2s",
		9*time.Second + 940*time.Millisecond: "9.9s",
		42 * time.Second:                     "42s",
		2*time.Minute + 5*time.Second:        "2m05s",
	}
	for duration, expected := range cases {
		if got := formatStageDuration(duration); got != expected {
			t.Fatalf("format(%s)=%q, want %q", duration, got, expected)
		}
	}
}

func TestStepperRendersDurationsRightAligned(t *testing.T) {
	current := populatedViewModel(t)
	current.activeStep = ""
	current.activeRole = ""
	current.activeCLI = ""
	base := time.Date(2026, 7, 24, 21, 0, 0, 0, time.UTC)
	current.now = fixedClock(base.Add(10 * time.Second))
	current.stages.stepStarted(pipeline.StepPlan, base)
	current.stages.stepFinished(
		pipeline.StepPlan,
		base.Add(1200*time.Millisecond),
	)

	rendered := renderStepper(current.theme, deriveStepper(current), 26)
	lines := strings.Split(rendered, "\n")
	if len(lines) != int(stageCount) {
		t.Fatalf("stepper rows=%d, want %d", len(lines), stageCount)
	}
	for index, line := range lines {
		if width := ansi.StringWidth(line); width > 26 {
			t.Fatalf("stepper row %d width=%d exceeds 26", index, width)
		}
	}
	plan := ansi.Strip(lines[stagePlan])
	if !strings.HasPrefix(plan, "✓ PLAN") || !strings.HasSuffix(plan, "1.2s") {
		t.Fatalf("plan row=%q", plan)
	}
	if got := ansi.StringWidth(lines[stagePlan]); got != 26 {
		t.Fatalf("plan row width=%d, want right-aligned to 26", got)
	}
}

func TestSidebarCardEmbedsTitleAndKeepsWidth(t *testing.T) {
	current := populatedViewModel(t)
	card := renderSidebarCard(
		current.theme,
		"PIPELINE",
		"one\n"+strings.Repeat("wide ", 20),
		30,
	)
	lines := strings.Split(card, "\n")
	if len(lines) != 4 {
		t.Fatalf("card rows=%d, want 4", len(lines))
	}
	for index, line := range lines {
		if width := ansi.StringWidth(line); width != 30 {
			t.Fatalf("card row %d width=%d, want 30", index, width)
		}
	}
	plain := ansi.Strip(lines[0])
	if !strings.HasPrefix(plain, "╭─ PIPELINE ") ||
		!strings.HasSuffix(plain, "╮") {
		t.Fatalf("card top border=%q", plain)
	}
}
