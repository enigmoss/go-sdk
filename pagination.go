package enigmoss

import (
	"context"
	"encoding/json"
	"iter"
)

// Page is one page of a list, newest first. To read the next page, set the query's Cursor
// to NextCursor; All walks every page for you.
type Page[T any] struct {
	Items []*T
	// NextCursor is the cursor of the next (older) page, "" on the last page.
	NextCursor string
	// HasMore reports whether there is a next page.
	HasMore bool
}

func decodePage[T any](resp *response) (*Page[T], error) {
	page := &Page[T]{}
	if err := decode(resp, &page.Items); err != nil {
		return nil, err
	}
	var meta struct {
		Cursor struct {
			Next    *string `json:"next"`
			HasMore bool    `json:"has_more"`
		} `json:"cursor"`
	}
	if len(resp.meta) > 0 {
		if err := json.Unmarshal(resp.meta, &meta); err != nil {
			return nil, unreadable(resp.status, resp.requestID, "The response meta does not match the API: "+err.Error()+".")
		}
	}
	if meta.Cursor.Next != nil {
		page.NextCursor = *meta.Cursor.Next
	}
	page.HasMore = meta.Cursor.HasMore && page.NextCursor != ""
	return page, nil
}

// all lazily walks the pages from cursor: the next page is fetched only when the previous
// one is consumed, and breaking out of the loop stops the requests.
func all[T any](ctx context.Context, cursor string, fetch func(ctx context.Context, cursor string) (*Page[T], error)) iter.Seq2[*T, error] {
	return func(yield func(*T, error) bool) {
		for {
			page, err := fetch(ctx, cursor)
			if err != nil {
				yield(nil, err)
				return
			}
			for _, item := range page.Items {
				if !yield(item, nil) {
					return
				}
			}
			if !page.HasMore {
				return
			}
			cursor = page.NextCursor
		}
	}
}

// first returns the first item of a page, or nil.
func first[T any](page *Page[T], err error) (*T, error) {
	if err != nil || len(page.Items) == 0 {
		return nil, err
	}
	return page.Items[0], nil
}
