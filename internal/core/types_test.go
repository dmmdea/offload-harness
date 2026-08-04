package core

import "testing"

// TestTaskOCRValid: the new ocr task type is recognized by Valid(), alongside the
// existing tasks; an unknown task is still rejected.
func TestTaskOCRValid(t *testing.T) {
	for _, task := range []TaskType{TaskSummarize, TaskClassify, TaskExtract, TaskTriage, TaskVQA, TaskOCR} {
		if !task.Valid() {
			t.Errorf("%q should be Valid()", task)
		}
	}
	if TaskType("nope").Valid() {
		t.Errorf("unknown task should not be Valid()")
	}
}

func TestTaskVideoDescribeValid(t *testing.T) {
	if !TaskVideoDescribe.Valid() {
		t.Fatal("TaskVideoDescribe should be Valid()")
	}
	if TaskType("nope").Valid() {
		t.Fatal("unknown task must be invalid")
	}
}

func TestTaskTranscribeValid(t *testing.T) {
	if !TaskTranscribe.Valid() {
		t.Fatal("TaskTranscribe should be Valid()")
	}
	if TaskType("nope-stt").Valid() {
		t.Fatal("unknown task must be invalid")
	}
}

// TestTaskGenerateVideoAudioValid: the two B1 generation task types are recognized
// by Valid() alongside the existing tasks; an unknown task is still rejected.
func TestTaskGenerateVideoAudioValid(t *testing.T) {
	if !TaskGenerateVideo.Valid() {
		t.Fatal("TaskGenerateVideo should be Valid()")
	}
	if !TaskGenerateAudio.Valid() {
		t.Fatal("TaskGenerateAudio should be Valid()")
	}
	if TaskType("nope-gen").Valid() {
		t.Fatal("unknown task must be invalid")
	}
}

// TestTaskPipelineJobValid (Task 4): the config-driven pipeline-job task type
// (internal/config Config.Pipelines, run generically via the task's configured
// script) is recognized by Valid() alongside the existing tasks.
func TestTaskPipelineJobValid(t *testing.T) {
	if !TaskPipelineJob.Valid() {
		t.Fatal("TaskPipelineJob should be Valid()")
	}
	if TaskType("nope-pipeline").Valid() {
		t.Fatal("unknown task must be invalid")
	}
}
