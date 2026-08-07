package appfinder

import "testing"

func TestChooseUsesTheFirstCompatibleInstalledApplication(t *testing.T) {
	applications := []Application{
		{ID: "word", Extensions: []string{"docx"}},
		{ID: "office-suite", Extensions: []string{"docx", "xlsx"}},
	}
	application, ok := choose("docx", applications)
	if !ok || application.ID != "word" {
		t.Fatalf("got %#v, %v", application, ok)
	}
}
