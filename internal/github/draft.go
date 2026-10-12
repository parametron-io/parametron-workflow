package github

import "context"

// DraftConverter grants only exact PR conversion, never review initiation.
type DraftConverter interface {
	ConvertPullRequestToDraft(context.Context, string) error
}

func (t *Transport) ConvertPullRequestToDraft(ctx context.Context, id string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !validIDs(id) {
		return failure(Permanent)
	}
	var data struct {
		Result *struct {
			PullRequest *struct {
				ID      string
				IsDraft *bool
			}
		}
	}
	err := t.query(ctx, `mutation($input:ConvertPullRequestToDraftInput!){result:convertPullRequestToDraft(input:$input){pullRequest{id isDraft}}}`, map[string]any{"input": map[string]any{"pullRequestId": id}}, &data)
	if err != nil {
		return err
	}
	if data.Result == nil || data.Result.PullRequest == nil || data.Result.PullRequest.ID != id || data.Result.PullRequest.IsDraft == nil || !*data.Result.PullRequest.IsDraft {
		return failure(Malformed)
	}
	return ctx.Err()
}

var _ DraftConverter = (*Transport)(nil)
