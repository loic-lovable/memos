package v1

import (
	"context"
	"strconv"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"

	v1pb "github.com/usememos/memos/proto/gen/api/v1"
	"github.com/usememos/memos/store"
)

// refuseNativeAdministration journals authenticated changes excluded by the
// pilot's fixed enrollment. No submitted values or credential bodies are stored.
func (s *APIV1Service) refuseNativeAdministration(ctx context.Context, action string) error {
	ctx, err := s.beginNativeAudit(ctx, action)
	if err != nil {
		return err
	}
	return s.finishNativeAudit(ctx, status.Error(codes.PermissionDenied, store.ErrShrimpNativeAdministration.Error()))
}

func (s *APIV1Service) beginNativeAudit(ctx context.Context, action string) (context.Context, error) {
	if !s.Store.ShrimpPilotEnabled() {
		return ctx, nil
	}
	user, err := s.fetchCurrentUser(ctx)
	if err != nil {
		return ctx, status.Error(codes.Internal, "failed to resolve audit actor")
	}
	if user == nil {
		return ctx, nil
	} // Unauthenticated attempts are outside the declared surface.
	audit, err := s.Store.BeginShrimpNativeAudit(ctx, "memos-user:"+strconv.FormatInt(int64(user.ID), 10), action)
	if err != nil {
		return ctx, status.Error(codes.Unavailable, "audit storage unavailable")
	}
	return audit, nil
}

func (s *APIV1Service) finishNativeAudit(ctx context.Context, result error) error {
	code := status.Code(result)
	knownRejection := code == codes.InvalidArgument || code == codes.NotFound || code == codes.PermissionDenied ||
		code == codes.Unauthenticated || code == codes.FailedPrecondition || code == codes.AlreadyExists
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := s.Store.FinishShrimpNativeAudit(ctx, code.String(), knownRejection); err != nil {
		return status.Error(codes.Unavailable, "audit storage unavailable")
	}
	return result
}

// UpdateUser records pilot attempts before processing native account edits.
func (s *APIV1Service) UpdateUser(ctx context.Context, request *v1pb.UpdateUserRequest) (response *v1pb.User, err error) {
	ctx, err = s.beginNativeAudit(ctx, "update_native_account")
	if err != nil {
		return nil, err
	}
	defer func() { err = s.finishNativeAudit(ctx, err) }()
	return s.updateUser(ctx, request)
}

// DeleteUser records pilot attempts before processing native account deletion.
func (s *APIV1Service) DeleteUser(ctx context.Context, request *v1pb.DeleteUserRequest) (response *emptypb.Empty, err error) {
	ctx, err = s.beginNativeAudit(ctx, "delete_native_account")
	if err != nil {
		return nil, err
	}
	defer func() { err = s.finishNativeAudit(ctx, err) }()
	return s.deleteUser(ctx, request)
}
