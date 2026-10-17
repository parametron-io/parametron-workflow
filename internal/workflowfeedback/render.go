package workflowfeedback

import "fmt"

const Marker = "<!-- parametron-workflow:managed-feedback:v1 -->"

type Content struct {
	Body   string
	Active bool
}

func reference(v Violation) string {
	return fmt.Sprintf("%s/%s#%d", v.related.Repository.Owner, v.related.Repository.Name, v.related.Number)
}
func Render(c Current) (Content, error) {
	list, err := canonical(c)
	if err != nil {
		return Content{}, err
	}
	body := Marker + "\n### Workflow feedback\n\n"
	if len(list) == 0 {
		return Content{Body: body + "Previously reported workflow constraints are resolved. No current rejection is reported."}, nil
	}
	for _, v := range list {
		var text string
		switch v.category {
		case BindingTarget:
			text = "PR binding was rejected (" + v.detail + "). The Target must identify an eligible Engineering Task, Feature, or explicitly branch-enabled Phase. Correct the `Target:` directive and satisfy the accepted development branch and author contract."
		case BindingBranch:
			text = "PR branch binding was rejected (" + v.detail + "). The head must match the current controller-authorized development branch with verified creation provenance. Use the accepted Target's authorized branch and satisfy the branch and author contract."
		case BindingDeveloper:
			text = "PR developer authorization was rejected (" + v.detail + "). The author must be a verified active organization member with repository write permission and match a manual assignee when assignees exist. Satisfy those requirements and the accepted Target and branch contract."
		case CompanionCapacity:
			text = "Engineering PR companion allocation was rejected for Target " + reference(v) + " (" + v.detail + " eligible PRs). Only nine eligible companions fit the Target's N+1 through N+9 capacity. Reduce eligible Engineering PR companions to nine or fewer."
		case Lifecycle:
			switch LifecycleReason(v.detail) {
			case ActiveBlocker:
				text = "Manual Status advancement or execution was rejected because a prerequisite remains open. Complete the blocking prerequisite before advancing Status or requesting execution."
			case InactiveParent:
				text = "Manual Status advancement or execution was rejected because the parent Phase is inactive. Wait until the parent Phase becomes active before advancing Status or requesting execution."
			case MissingExecutionAuthorization:
				text = "Manual Status advancement or execution was rejected because current execution authorization is missing. Satisfy current lifecycle prerequisites and obtain controller-authorized development activity before advancing Status."
			}
		case PhaseClosure:
			text = "Phase closure was rejected or reverted because direct child Issues remain open. Close all required direct child Issues before attempting Phase closure again."
		case Development:
			text = "A contradictory manual Development relationship was selected for correction. Development must follow the validated Target binding to " + reference(v) + ". Use that accepted `Target:` binding instead of manually assigning a contradictory Development relationship."
		case Terminal:
			if v.detail == "unauthorized_native_closure" {
				text = "Native Issue closure was rejected or reversed because applicable terminal authorization was missing. Task/Feature closure requires a verified accepted implementation-PR merge or a newly created `Force-Close: <nonempty reason>` comment immediately followed by the same actor's native close in the current cycle. A Phase requires explicit native closure with every direct child closed; implementation-PR closure cannot complete the Phase."
			} else {
				text = "Project Done was selected for correction because the Issue is natively open. Done is a projection of authorized terminal state and cannot authorize completion. Complete the applicable native closure contract; the controller restores the current canonical open Status."
			}
		}
		if text == "" {
			return Content{}, ErrInvalid
		}
		body += "- " + text + "\n"
	}
	if len(body) > 60000 {
		return Content{}, ErrInvalid
	}
	return Content{Body: body, Active: true}, nil
}
