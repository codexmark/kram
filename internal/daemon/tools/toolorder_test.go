package tools

import (
	"reflect"
	"testing"
)

func TestValidateToolOrderNilIsValid(t *testing.T) {
	if err := ValidateToolOrder(nil); err != nil {
		t.Errorf("nil order should be valid (means today's plain alphabetical behavior), got %v", err)
	}
}

func TestValidateToolOrderRequiresRestMarker(t *testing.T) {
	err := ValidateToolOrder([]string{"bash", "edit_file"})
	if err == nil {
		t.Fatal("expected an error when the rest marker is missing")
	}
}

func TestValidateToolOrderRejectsDuplicates(t *testing.T) {
	err := ValidateToolOrder([]string{"bash", "bash", ToolOrderRest})
	if err == nil {
		t.Fatal("expected an error for a duplicate entry")
	}
}

func TestValidateToolOrderAcceptsWellFormedList(t *testing.T) {
	err := ValidateToolOrder([]string{"bash", "edit_file", ToolOrderRest})
	if err != nil {
		t.Errorf("expected a well-formed order to validate, got %v", err)
	}
}

func TestUnknownToolOrderNamesFindsTypos(t *testing.T) {
	known := map[string]bool{"bash": true, "edit_file": true}
	unknown := UnknownToolOrderNames([]string{"bash", "edit_fiel", ToolOrderRest}, known)
	if !reflect.DeepEqual(unknown, []string{"edit_fiel"}) {
		t.Errorf("unknown = %v, want [edit_fiel]", unknown)
	}
}

func TestUnknownToolOrderNamesIgnoresRestMarker(t *testing.T) {
	known := map[string]bool{"bash": true}
	unknown := UnknownToolOrderNames([]string{"bash", ToolOrderRest}, known)
	if len(unknown) != 0 {
		t.Errorf("expected no unknown names, got %v", unknown)
	}
}

func TestOrderToolNamesNilOrderPreservesInput(t *testing.T) {
	visible := []string{"bash", "edit_file", "grep"}
	got := OrderToolNames(visible, nil)
	if !reflect.DeepEqual(got, visible) {
		t.Errorf("OrderToolNames(visible, nil) = %v, want it unchanged: %v", got, visible)
	}
}

func TestOrderToolNamesAppliesExplicitOrderWithRestAlphabetical(t *testing.T) {
	visible := []string{"bash", "edit_file", "grep", "glob", "run_background"}
	order := []string{"run_background", ToolOrderRest, "bash"}
	got := OrderToolNames(visible, order)
	want := []string{"run_background", "edit_file", "glob", "grep", "bash"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("OrderToolNames(...) = %v, want %v", got, want)
	}
}

func TestOrderToolNamesRestOnlyIsPlainAlphabetical(t *testing.T) {
	visible := []string{"grep", "bash", "edit_file"}
	order := []string{ToolOrderRest}
	got := OrderToolNames(visible, order)
	want := []string{"bash", "edit_file", "grep"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("OrderToolNames(rest-only) = %v, want alphabetical %v", got, want)
	}
}

func TestOrderToolNamesListedButNotVisibleIsOmitted(t *testing.T) {
	// bash is listed in order but not currently visible (e.g. disabled) —
	// it must not appear in the result, same as an unordered overview.
	visible := []string{"edit_file", "grep"}
	order := []string{"bash", ToolOrderRest}
	got := OrderToolNames(visible, order)
	want := []string{"edit_file", "grep"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("OrderToolNames(...) = %v, want %v (bash omitted, not visible)", got, want)
	}
}
