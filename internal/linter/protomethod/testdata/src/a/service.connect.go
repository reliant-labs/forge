package testv1

import (
	context "context"
)

// TestServiceHandler is the proto-generated handler interface.
// In real code this lives in a *.connect.go file.
type TestServiceHandler interface {
	CreateItem(context.Context, *CreateItemRequest) (*Item, error)
	GetItem(context.Context, *GetItemRequest) (*Item, error)
	ListItems(context.Context, *ListItemsRequest) (*ListItemsResponse, error)
}

// UnimplementedTestServiceHandler provides default implementations.
type UnimplementedTestServiceHandler struct{}

func (UnimplementedTestServiceHandler) CreateItem(context.Context, *CreateItemRequest) (*Item, error) {
	return nil, nil
}

func (UnimplementedTestServiceHandler) GetItem(context.Context, *GetItemRequest) (*Item, error) {
	return nil, nil
}

func (UnimplementedTestServiceHandler) ListItems(context.Context, *ListItemsRequest) (*ListItemsResponse, error) {
	return nil, nil
}

type CreateItemRequest struct{}
type GetItemRequest struct{}
type ListItemsRequest struct{}
type Item struct{}
type ListItemsResponse struct{}
