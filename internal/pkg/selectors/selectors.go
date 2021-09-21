// Package selectors contains exceptions related utilities, including check the validity of selector,
// treeSelector, and noneSelector, and parsing these three selectors.
package selectors

import (
	"fmt"
	"strconv"
	"strings"

	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/validation"

	api "sigs.k8s.io/hierarchical-namespaces/api/v1alpha2"
)

func ShouldPropagate(inst *unstructured.Unstructured, nsLabels labels.Set) (bool, error) {
	if sel, err := GetSelector(inst); err != nil {
		return false, err
	} else if sel != nil && !sel.Matches(nsLabels) {
		return false, nil
	}
	if sel, err := GetTreeSelector(inst); err != nil {
		return false, err
	} else if sel != nil && !sel.Matches(nsLabels) {
		return false, nil
	}
	if none, err := GetNoneSelector(inst); err != nil || none {
		return false, err
	}
	if excluded, err := isExcluded(inst); excluded {
		return false, err
	}
	return true, nil
}

func GetSelectorAnnotation(inst *unstructured.Unstructured) string {
	annot := inst.GetAnnotations()
	return annot[api.AnnotationSelector]
}

func GetTreeSelectorAnnotation(inst *unstructured.Unstructured) string {
	annot := inst.GetAnnotations()
	return annot[api.AnnotationTreeSelector]
}

func GetNoneSelectorAnnotation(inst *unstructured.Unstructured) string {
	annot := inst.GetAnnotations()
	return annot[api.AnnotationNoneSelector]
}

// GetTreeSelector is similar to a regular selector, except that it adds the LabelTreeDepthSuffix to every string
// To transform a tree selector into a regular label selector, we follow these steps:
// 1. get the treeSelector annotation if it exists
// 2. convert the annotation string to a slice of strings seperated by comma, because user is allowed to put multiple selectors
// 3. append the LabelTreeDepthSuffix to each of the treeSelector string
// 4. combine them into a single string connected by comma
func GetTreeSelector(inst *unstructured.Unstructured) (labels.Selector, error) {
	treeSelectorStr := GetTreeSelectorAnnotation(inst)
	if treeSelectorStr == "" {
		return nil, nil
	}

	segs := strings.Split(treeSelectorStr, ",")
	selectorStr := ""
	nonNegatedNses := []string{}
	for i, seg := range segs {
		seg = strings.TrimSpace(seg)
		// check if it's a valid namespace name
		if err := validateTreeSelectorSegment(seg); err != nil {
			return nil, err
		}

		if seg[0] != '!' {
			nonNegatedNses = append(nonNegatedNses, seg)
		}

		selectorStr = selectorStr + seg + api.LabelTreeDepthSuffix
		if i < len(segs)-1 {
			selectorStr = selectorStr + ", "
		}
	}

	treeSelector, err := getSelectorFromString(selectorStr)
	if err != nil {
		// In theory this should never happen because we already checked DNS label before.
		// If this happens, it's more likely that we have a bug in our code
		return nil, fmt.Errorf("internal error while parsing %q: %w", api.AnnotationTreeSelector, err)
	}

	// If there are more than one non-negated namespace, this object will not be propagated to
	// any child namespace. We want to warn and stop user from doing this.
	if len(nonNegatedNses) > 1 {
		return nil, fmt.Errorf("should only have one non-negated namespace, but got multiple: %v", nonNegatedNses)
	}
	return treeSelector, nil
}

func validateTreeSelectorSegment(seg string) error {
	seg = strings.TrimSpace(seg)
	ns := ""
	if seg[0] == '!' {
		ns = seg[1:]
	} else {
		ns = seg
	}
	errStrs := validation.IsDNS1123Label(ns)
	if len(errStrs) != 0 {
		// If IsDNS1123Label() returns multiple errors, it will look like:
		// "ns" is not a valid namespace name: err1; err2
		return fmt.Errorf("%q is not a valid namespace name: %s", ns, strings.Join(errStrs, "; "))
	}
	return nil
}

// GetSelector returns the selector on a given object if it exists
func GetSelector(inst *unstructured.Unstructured) (labels.Selector, error) {
	selector, err := getSelectorFromString(GetSelectorAnnotation(inst))
	if err != nil {
		return nil, fmt.Errorf("while parsing %q: %w", api.AnnotationSelector, err)
	}
	return selector, nil
}

// getSelectorFromString converts the given string to a selector
// Note: any invalid Selector value will cause this object not propagating to any child namespace
func getSelectorFromString(str string) (labels.Selector, error) {
	labelSelector, err := v1.ParseToLabelSelector(str)
	if err != nil {
		return nil, err
	}
	selector, err := v1.LabelSelectorAsSelector(labelSelector)
	if err != nil {
		return nil, err
	}
	return selector, nil
}

// GetNoneSelector returns true indicates that user do not want this object to be propagated
func GetNoneSelector(inst *unstructured.Unstructured) (bool, error) {
	noneSelectorStr := GetNoneSelectorAnnotation(inst)
	// Empty string is treated as 'false'. In other selector cases, the empty string is auto converted to
	// a selector that matches everything.
	if noneSelectorStr == "" {
		return false, nil
	}
	noneSelector, err := strconv.ParseBool(noneSelectorStr)
	if err != nil {
		// Returning false here in accordance to the Go standard
		return false,
			fmt.Errorf("invalid %s value: %w",
				api.AnnotationNoneSelector,
				err,
			)

	}
	return noneSelector, nil
}

// cmExclusions are known (istio and kube-root) CA configmap which are excluded from propagation
var cmExclusions = []string{"istio-ca-root-cert", "kube-root-ca.crt"}

// isExcluded returns true to indicate that this object is excluded from being propagated
func isExcluded(inst *unstructured.Unstructured) (bool, error) {
	name := inst.GetName()
	kind := inst.GetKind()
	group := inst.GroupVersionKind().Group

	for _, excludedResourceName := range cmExclusions {
		if group == "" && kind == "ConfigMap" && name == excludedResourceName {
			return true, nil
		}
	}
	return false, nil
}
