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
