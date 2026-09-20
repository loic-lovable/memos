package store

import "context"

// UserIdentity is the linkage between an external identity subject and a local user.
// Uniqueness is enforced on (Provider, ExternUID); one local user may have multiple
// identities across different providers.
type UserIdentity struct {
	ID        int32
	UserID    int32
	Provider  string
	ExternUID string
	CreatedTs int64
	UpdatedTs int64
}

// FindUserIdentity is used to filter user identities in list/get queries.
type FindUserIdentity struct {
	ID        *int32
	UserID    *int32
	Provider  *string
	ExternUID *string
}

// DeleteUserIdentity is used to delete user identity linkage rows.
type DeleteUserIdentity struct {
	ID       *int32
	UserID   *int32
	Provider *string
}

// CreateUserIdentity creates a new external-identity linkage record.
// A unique-constraint violation is returned as ErrUserIdentityTaken; callers
// are responsible for reconciling concurrent first-login races on
// (Provider, ExternUID).
func (s *Store) CreateUserIdentity(ctx context.Context, create *UserIdentity) (*UserIdentity, error) {
	if s.shrimpPilot {
		s.admissionMu.Lock()
		defer s.admissionMu.Unlock()
		if err := s.checkUnmanagedIdentity(ctx, create.UserID); err != nil {
			return nil, err
		}
	}
	identity, err := s.driver.CreateUserIdentity(ctx, create)
	if err != nil {
		if uniqueErr := classifyUserUniqueViolation(err); uniqueErr != nil {
			return nil, uniqueErr
		}
		return nil, err
	}
	return identity, nil
}

// CreateUserWithIdentity atomically creates a local user and its external identity
// linkage, returning the created user. Unique-constraint violations are returned
// as ErrUsernameTaken, ErrEmailTaken, or ErrUserIdentityTaken so the caller can
// tell which conflict it hit.
func (s *Store) CreateUserWithIdentity(ctx context.Context, createUser *User, createIdentity *UserIdentity) (*User, error) {
	email, err := normalizeUserEmail(createUser.Email)
	if err != nil {
		return nil, err
	}
	createUser.Email = email
	user, err := s.driver.CreateUserWithIdentity(ctx, createUser, createIdentity)
	if err != nil {
		if uniqueErr := classifyUserUniqueViolation(err); uniqueErr != nil {
			return nil, uniqueErr
		}
		return nil, err
	}
	s.userCache.Set(ctx, userCacheKey(user.ID), user)
	return user, nil
}

// ListUserIdentities returns all linkage records matching the filter.
func (s *Store) ListUserIdentities(ctx context.Context, find *FindUserIdentity) ([]*UserIdentity, error) {
	return s.driver.ListUserIdentities(ctx, find)
}

// DeleteUserIdentities deletes all linkage records matching the filter.
func (s *Store) DeleteUserIdentities(ctx context.Context, delete *DeleteUserIdentity) error {
	if s.shrimpPilot {
		s.admissionMu.Lock()
		defer s.admissionMu.Unlock()
		identities, err := s.driver.ListUserIdentities(ctx, &FindUserIdentity{ID: delete.ID, UserID: delete.UserID, Provider: delete.Provider})
		if err != nil {
			return err
		}
		for _, identity := range identities {
			if err := s.checkUnmanagedIdentity(ctx, identity.UserID); err != nil {
				return err
			}
		}
	}
	return s.driver.DeleteUserIdentities(ctx, delete)
}

// Caller holds admissionMu against provisioned account changes and publication.
func (s *Store) checkUnmanagedIdentity(ctx context.Context, userID int32) error {
	_, revision, err := s.driver.(ShrimpDriver).ShrimpAdmission(ctx, userID)
	if err != nil {
		return err
	}
	if revision != "" {
		return ErrShrimpManagedIdentity
	}
	return nil
}

// GetUserIdentity returns the first linkage record matching the filter, or nil if none found.
func (s *Store) GetUserIdentity(ctx context.Context, find *FindUserIdentity) (*UserIdentity, error) {
	list, err := s.ListUserIdentities(ctx, find)
	if err != nil {
		return nil, err
	}
	if len(list) == 0 {
		return nil, nil
	}
	return list[0], nil
}
