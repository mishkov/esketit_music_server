package main

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"
)

// domainState is a request- or transaction-local view used by algorithms that
// need to correlate denormalized domain rows. It is never retained by
// trackStore and is discarded when the operation returns.
type domainState struct {
	*trackStore
	mu            sync.RWMutex
	ctx           context.Context
	repositories  domainRepositories
	tracks        map[int64]track
	albums        map[int64]album
	authors       map[int64]author
	users         map[int64]user
	usersByEmail  map[string]int64
	playlists     map[int64]playlist
	lyricsByTrack map[int64]lyrics
}

type stateDomain uint32

const (
	stateTracks stateDomain = 1 << iota
	stateAlbums
	stateAuthors
	stateUsers
	statePlaylists
	stateLyrics
	stateCatalog = stateTracks | stateAlbums | stateAuthors
	stateAll     = stateCatalog | stateUsers | statePlaylists | stateLyrics
)

func loadDomainState(ctx context.Context, store *trackStore, repositories domainRepositories, domains stateDomain) (*domainState, error) {
	state := newDomainStateView(ctx, store, repositories)
	if domains&stateTracks != 0 {
		items, err := repositories.catalog.ListTracks(ctx)
		if err != nil {
			return nil, err
		}
		addTracksToState(state, items)
	}
	if domains&stateAlbums != 0 {
		items, err := repositories.catalog.ListAlbums(ctx)
		if err != nil {
			return nil, err
		}
		addAlbumsToState(state, items)
	}
	if domains&stateAuthors != 0 {
		items, err := repositories.authors.List(ctx)
		if err != nil {
			return nil, err
		}
		addAuthorsToState(state, items)
	}
	if domains&stateUsers != 0 {
		items, err := repositories.users.List(ctx)
		if err != nil {
			return nil, err
		}
		for _, item := range items {
			state.users[item.ID] = item
			state.usersByEmail[normalizeEmail(item.Email)] = item.ID
		}
	}
	if domains&statePlaylists != 0 {
		items, err := repositories.playlists.List(ctx)
		if err != nil {
			return nil, err
		}
		addPlaylistsToState(state, items)
	}
	if domains&stateLyrics != 0 {
		items, err := repositories.lyrics.List(ctx)
		if err != nil {
			return nil, err
		}
		for _, item := range items {
			state.lyricsByTrack[item.TrackID] = item
		}
	}
	return state, nil
}

func newDomainStateView(ctx context.Context, store *trackStore, repositories domainRepositories) *domainState {
	return &domainState{
		trackStore: store, ctx: ctx, repositories: repositories,
		tracks: make(map[int64]track), albums: make(map[int64]album), authors: make(map[int64]author),
		users: make(map[int64]user), usersByEmail: make(map[string]int64),
		playlists: make(map[int64]playlist), lyricsByTrack: make(map[int64]lyrics),
	}
}

func addTracksToState(state *domainState, items []track) {
	for _, item := range items {
		state.tracks[item.ID] = item
	}
}

func addAlbumsToState(state *domainState, items []album) {
	for _, item := range items {
		state.albums[item.ID] = item
	}
}

func addAuthorsToState(state *domainState, items []author) {
	for _, item := range items {
		state.authors[item.ID] = item
	}
}

func addPlaylistsToState(state *domainState, items []playlist) {
	for _, item := range items {
		state.playlists[item.ID] = item
	}
}

func paginationMetadata(requestedPage, requestedPageSize, totalItems int) (int, int, int) {
	page := normalizePage(requestedPage)
	pageSize := normalizePageSize(requestedPageSize)
	totalPages := 0
	if totalItems > 0 {
		totalPages = (totalItems + pageSize - 1) / pageSize
	}
	return page, pageSize, totalPages
}

func buildPlaylistResponse(item playlist) playlistResponse {
	return playlistResponse{
		ID: item.ID, UserID: item.UserID, Name: item.Name, Description: item.Description,
		CoverImagePath: item.CoverImagePath, Visibility: item.Visibility,
		TrackCount: len(item.TrackItems), System: item.System, Kind: item.Kind,
		IsFavorites: item.Kind == playlistKindFavorites, ShareToken: item.ShareToken,
	}
}

func cloneAlbumForRead(item album) album {
	item.AuthorIDs = append([]int64(nil), item.AuthorIDs...)
	item.TrackIDs = append([]int64(nil), item.TrackIDs...)
	item.AdditionalInfo = normalizeAdditionalInfo(item.AdditionalInfo)
	return item
}

func (s *trackStore) withinReadState(domains stateDomain, operation func(*domainState) error) error {
	ctx := context.Background()
	return s.unitOfWork.WithinReadTransaction(ctx, func(repositories domainRepositories) error {
		state, err := loadDomainState(ctx, s, repositories, domains)
		if err != nil {
			return err
		}
		return operation(state)
	})
}

func (s *trackStore) withinReadTransaction(operation func(context.Context, domainRepositories) error) error {
	ctx := context.Background()
	return s.unitOfWork.WithinReadTransaction(ctx, func(repositories domainRepositories) error {
		return operation(ctx, repositories)
	})
}

func (s *trackStore) withinStateTransaction(domains stateDomain, operation func(*domainState) error) error {
	return s.withinStateTransactionContext(context.Background(), domains, operation)
}

func (s *trackStore) withinStateTransactionContext(ctx context.Context, domains stateDomain, operation func(*domainState) error) error {
	return s.unitOfWork.WithinTransaction(ctx, func(repositories domainRepositories) error {
		state, err := loadDomainState(ctx, s, repositories, domains)
		if err != nil {
			return fmt.Errorf("load transaction state: %w", err)
		}
		return operation(state)
	})
}

func (s *trackStore) list() ([]track, error) {
	items, err := s.catalogRepository.ListTracks(context.Background())
	if err != nil {
		return nil, fmt.Errorf("list tracks: %w", err)
	}
	return items, nil
}

func (s *trackStore) listAlbums(filter albumListFilter) (paginatedAlbums, error) {
	var items []album
	var total int
	err := s.withinReadTransaction(func(ctx context.Context, repositories domainRepositories) error {
		var err error
		items, total, err = repositories.reads.ListAlbumsPage(ctx, filter)
		return err
	})
	if err != nil {
		return paginatedAlbums{}, fmt.Errorf("list albums: %w", err)
	}
	page, pageSize, totalPages := paginationMetadata(filter.Page, filter.PageSize, total)
	return paginatedAlbums{Items: items, Page: page, PageSize: pageSize, TotalItems: total, TotalPages: totalPages}, nil
}

func (s *trackStore) getAlbum(id int64) (album, bool, error) {
	item, ok, err := s.catalogRepository.FindAlbumByID(context.Background(), id)
	if err != nil {
		return album{}, false, fmt.Errorf("get album: %w", err)
	}
	return item, ok, nil
}

func (s *trackStore) getAlbumTracks(id, userID int64) ([]trackResponse, bool, error) {
	var items []trackResponse
	var found bool
	err := s.withinReadTransaction(func(ctx context.Context, repositories domainRepositories) error {
		albumItem, ok, err := repositories.catalog.FindAlbumByID(ctx, id)
		if err != nil || !ok {
			return err
		}
		found = true
		tracks, err := repositories.reads.ListTracksByIDs(ctx, albumItem.TrackIDs)
		if err != nil {
			return err
		}
		preferences, err := repositories.reads.ListPreferencePlaylists(ctx, userID)
		if err != nil {
			return err
		}
		state := newDomainStateView(ctx, s, repositories)
		addAlbumsToState(state, []album{albumItem})
		addTracksToState(state, tracks)
		addPlaylistsToState(state, preferences)
		items, _ = state.getAlbumTracks(id, userID)
		return nil
	})
	if err != nil {
		return nil, false, fmt.Errorf("get album tracks: %w", err)
	}
	return items, found, nil
}

func (s *trackStore) get(id int64) (track, bool, error) {
	item, ok, err := s.catalogRepository.FindTrackByID(context.Background(), id)
	if err != nil {
		return track{}, false, fmt.Errorf("get track: %w", err)
	}
	return item, ok, nil
}

func (s *trackStore) createAlbum(req upsertAlbumRequest) (result album, returnErr error) {
	returnErr = s.withinStateTransaction(stateCatalog, func(state *domainState) error {
		var err error
		result, err = state.createAlbum(req)
		return err
	})
	return
}

func (s *trackStore) updateAlbum(id int64, req upsertAlbumRequest) (result album, found bool, returnErr error) {
	returnErr = s.withinStateTransaction(stateCatalog, func(state *domainState) error {
		var err error
		result, found, err = state.updateAlbum(id, req)
		return err
	})
	return
}

func (s *trackStore) deleteAlbum(id int64) (deleted bool, returnErr error) {
	returnErr = s.withinStateTransaction(stateAlbums, func(state *domainState) error {
		var err error
		deleted, err = state.deleteAlbum(id)
		return err
	})
	return
}

func (s *trackStore) create(req upsertTrackRequest) (result track, returnErr error) {
	return s.createWithContext(context.Background(), req)
}

func (s *trackStore) createWithContext(ctx context.Context, req upsertTrackRequest) (result track, returnErr error) {
	returnErr = s.withinStateTransactionContext(ctx, stateCatalog, func(state *domainState) error {
		var err error
		result, err = state.create(req)
		return err
	})
	return
}

func (s *trackStore) update(id int64, req upsertTrackRequest) (result track, found bool, returnErr error) {
	returnErr = s.withinStateTransaction(stateCatalog, func(state *domainState) error {
		var err error
		result, found, err = state.update(id, req)
		return err
	})
	return
}

func (s *trackStore) delete(id int64) (deleted bool, returnErr error) {
	returnErr = s.withinStateTransaction(stateAll, func(state *domainState) error {
		var err error
		deleted, err = state.delete(id)
		return err
	})
	return
}

func (s *trackStore) listPlaylists(userID int64, filter playlistListFilter) (paginatedPlaylists, error) {
	var playlists []playlist
	var total int
	err := s.withinReadTransaction(func(ctx context.Context, repositories domainRepositories) error {
		var err error
		playlists, total, err = repositories.reads.ListPlaylistsPage(ctx, userID, filter)
		return err
	})
	if err != nil {
		return paginatedPlaylists{}, fmt.Errorf("list playlists: %w", err)
	}
	items := make([]playlistResponse, 0, len(playlists))
	for _, item := range playlists {
		items = append(items, buildPlaylistResponse(item))
	}
	page, pageSize, totalPages := paginationMetadata(filter.Page, filter.PageSize, total)
	return paginatedPlaylists{Items: items, Page: page, PageSize: pageSize, TotalItems: total, TotalPages: totalPages}, nil
}

func (s *trackStore) getPlaylist(userID, playlistID int64) (playlistResponse, bool, error) {
	item, ok, err := s.playlistRepository.FindByID(context.Background(), playlistID)
	if err != nil {
		return playlistResponse{}, false, fmt.Errorf("get playlist: %w", err)
	}
	if !ok || item.UserID != userID {
		return playlistResponse{}, false, nil
	}
	return buildPlaylistResponse(item), true, nil
}

func (s *trackStore) createPlaylist(userID int64, req upsertPlaylistRequest) (result playlistResponse, returnErr error) {
	returnErr = s.withinStateTransaction(stateUsers|statePlaylists, func(state *domainState) error {
		var err error
		result, err = state.createPlaylist(userID, req)
		return err
	})
	return
}

func (s *trackStore) updatePlaylist(userID, playlistID int64, req upsertPlaylistRequest) (result playlistResponse, found bool, returnErr error) {
	returnErr = s.withinStateTransaction(statePlaylists, func(state *domainState) error {
		var err error
		result, found, err = state.updatePlaylist(userID, playlistID, req)
		return err
	})
	return
}

func (s *trackStore) validatePlaylistCoverUploadTarget(userID, playlistID int64) (bool, error) {
	item, ok, err := s.playlistRepository.FindByID(context.Background(), playlistID)
	if err != nil {
		return false, err
	}
	if !ok || item.UserID != userID {
		return false, nil
	}
	if item.System {
		return true, errSystemPlaylistImmutable
	}
	return true, nil
}

func (s *trackStore) updatePlaylistCoverImage(userID, playlistID int64, path string) (result playlistResponse, found bool, returnErr error) {
	returnErr = s.withinStateTransaction(statePlaylists, func(state *domainState) error {
		var err error
		result, found, err = state.updatePlaylistCoverImage(userID, playlistID, path)
		return err
	})
	return
}

func (s *trackStore) deletePlaylist(userID, playlistID int64) (deleted bool, returnErr error) {
	returnErr = s.withinStateTransaction(statePlaylists, func(state *domainState) error {
		var err error
		deleted, err = state.deletePlaylist(userID, playlistID)
		return err
	})
	return
}

func (s *trackStore) getPlaylistTracks(userID, playlistID, page, pageSize int64) (paginatedTracks, bool, error) {
	return s.readPlaylistTracks("get playlist tracks", userID, page, pageSize, func(ctx context.Context, repositories domainRepositories) (playlist, bool, error) {
		item, ok, err := repositories.playlists.FindByID(ctx, playlistID)
		if err != nil || !ok || item.UserID != userID {
			return playlist{}, false, err
		}
		return item, true, nil
	})
}

func (s *trackStore) getPublicPlaylist(playlistID int64) (playlistResponse, bool, error) {
	item, ok, err := s.playlistRepository.FindByID(context.Background(), playlistID)
	if err != nil {
		return playlistResponse{}, false, fmt.Errorf("get public playlist: %w", err)
	}
	if !ok || item.Visibility != playlistVisibilityPublic {
		return playlistResponse{}, false, nil
	}
	return publicPlaylistResponse(buildPlaylistResponse(item)), true, nil
}

func (s *trackStore) getPublicPlaylistTracks(playlistID, userID, page, pageSize int64) (paginatedTracks, bool, error) {
	return s.readPlaylistTracks("get public playlist tracks", userID, page, pageSize, func(ctx context.Context, repositories domainRepositories) (playlist, bool, error) {
		item, ok, err := repositories.playlists.FindByID(ctx, playlistID)
		if err != nil || !ok || item.Visibility != playlistVisibilityPublic {
			return playlist{}, false, err
		}
		return item, true, nil
	})
}

func (s *trackStore) getSharedPlaylist(token string) (playlistResponse, bool, error) {
	var item playlist
	var found bool
	err := s.withinReadTransaction(func(ctx context.Context, repositories domainRepositories) error {
		var err error
		item, found, err = repositories.reads.FindPlaylistByShareToken(ctx, token)
		return err
	})
	if err != nil {
		return playlistResponse{}, false, fmt.Errorf("get shared playlist: %w", err)
	}
	if !found {
		return playlistResponse{}, false, nil
	}
	return publicPlaylistResponse(buildPlaylistResponse(item)), true, nil
}

func (s *trackStore) getSharedPlaylistTracks(token string, userID, page, pageSize int64) (paginatedTracks, bool, error) {
	return s.readPlaylistTracks("get shared playlist tracks", userID, page, pageSize, func(ctx context.Context, repositories domainRepositories) (playlist, bool, error) {
		return repositories.reads.FindPlaylistByShareToken(ctx, token)
	})
}

type playlistReadTarget func(context.Context, domainRepositories) (playlist, bool, error)

func (s *trackStore) readPlaylistTracks(operation string, userID, page, pageSize int64, find playlistReadTarget) (paginatedTracks, bool, error) {
	var result paginatedTracks
	var found bool
	err := s.withinReadTransaction(func(ctx context.Context, repositories domainRepositories) error {
		playlistItem, ok, err := find(ctx, repositories)
		if err != nil || !ok {
			return err
		}
		found = true
		normalizedPage, normalizedPageSize, totalPages := paginationMetadata(int(page), int(pageSize), len(playlistItem.TrackItems))
		start := (normalizedPage - 1) * normalizedPageSize
		if start > len(playlistItem.TrackItems) {
			start = len(playlistItem.TrackItems)
		}
		end := start + normalizedPageSize
		if end > len(playlistItem.TrackItems) {
			end = len(playlistItem.TrackItems)
		}
		pageItems := playlistItem.TrackItems[start:end]
		trackIDs := make([]int64, 0, len(pageItems))
		for _, item := range pageItems {
			trackIDs = append(trackIDs, item.TrackID)
		}
		tracks, err := repositories.reads.ListTracksByIDs(ctx, trackIDs)
		if err != nil {
			return err
		}
		albumIDs := make([]int64, 0, len(pageItems))
		currentTracks := make(map[int64]track, len(tracks))
		for _, item := range tracks {
			currentTracks[item.ID] = item
			albumIDs = append(albumIDs, item.AlbumID)
		}
		for _, item := range pageItems {
			if _, ok := currentTracks[item.TrackID]; !ok && item.UnavailableTrack != nil {
				albumIDs = append(albumIDs, item.UnavailableTrack.AlbumID)
			}
		}
		albums, err := repositories.reads.ListAlbumsByIDs(ctx, albumIDs)
		if err != nil {
			return err
		}
		preferences, err := repositories.reads.ListPreferencePlaylists(ctx, userID)
		if err != nil {
			return err
		}
		state := newDomainStateView(ctx, s, repositories)
		addTracksToState(state, tracks)
		addAlbumsToState(state, albums)
		addPlaylistsToState(state, preferences)
		favoriteIDs := state.favoriteTrackSetLocked(userID)
		dislikedIDs := state.dislikedTrackSetLocked(userID)
		responses := make([]trackResponse, 0, len(pageItems))
		for _, item := range pageItems {
			responses = append(responses, state.buildPlaylistTrackResponseLocked(item, favoriteIDs, dislikedIDs))
		}
		result = paginatedTracks{
			Items: responses, Page: normalizedPage, PageSize: normalizedPageSize,
			TotalItems: len(playlistItem.TrackItems), TotalPages: totalPages,
		}
		return nil
	})
	if err != nil {
		return paginatedTracks{}, false, fmt.Errorf("%s: %w", operation, err)
	}
	return result, found, nil
}

func (s *trackStore) addTrackToPlaylists(userID, trackID int64, playlistIDs []int64) error {
	return s.withinStateTransaction(stateTracks|statePlaylists, func(state *domainState) error {
		return state.addTrackToPlaylists(userID, trackID, playlistIDs)
	})
}

func (s *trackStore) removeTrackFromPlaylist(userID, playlistID, trackID int64) (removed bool, returnErr error) {
	returnErr = s.withinStateTransaction(statePlaylists, func(state *domainState) error {
		var err error
		removed, err = state.removeTrackFromPlaylist(userID, playlistID, trackID)
		return err
	})
	return
}

func (s *trackStore) setFavoriteTrack(userID, trackID int64, favorite bool) error {
	return s.withinStateTransaction(stateTracks|statePlaylists, func(state *domainState) error {
		return state.setFavoriteTrack(userID, trackID, favorite)
	})
}

func (s *trackStore) setDislikedTrack(userID, trackID int64, disliked bool) error {
	return s.withinStateTransaction(stateTracks|statePlaylists, func(state *domainState) error {
		return state.setDislikedTrack(userID, trackID, disliked)
	})
}

func (s *trackStore) nextAutoplayTracks(userID int64, req autoplayNextRequest) (autoplayNextResponse, error) {
	var result autoplayNextResponse
	err := s.withinReadState(stateCatalog|stateUsers|statePlaylists, func(state *domainState) error {
		var err error
		result, err = state.nextAutoplayTracks(userID, req)
		return err
	})
	return result, err
}

func (s *trackStore) reorderPlaylistTracks(userID, playlistID int64, trackIDs []int64) (found bool, returnErr error) {
	returnErr = s.withinStateTransaction(statePlaylists, func(state *domainState) error {
		var err error
		found, err = state.reorderPlaylistTracks(userID, playlistID, trackIDs)
		return err
	})
	return
}

func (s *trackStore) listAuthors(filter authorListFilter) ([]author, error) {
	var items []author
	err := s.withinReadState(stateAuthors, func(state *domainState) error {
		var err error
		items, err = state.listAuthors(filter)
		return err
	})
	return items, err
}

func (s *trackStore) search(userID int64, filter searchListFilter) (paginatedSearchResults, error) {
	var items []searchResultItem
	var total int
	err := s.withinReadTransaction(func(ctx context.Context, repositories domainRepositories) error {
		refs, count, err := repositories.reads.SearchPage(ctx, userID, filter)
		if err != nil {
			return err
		}
		total = count
		authorIDs := make([]int64, 0, len(refs))
		albumIDs := make([]int64, 0, len(refs))
		trackIDs := make([]int64, 0, len(refs))
		playlistIDs := make([]int64, 0, len(refs))
		for _, ref := range refs {
			switch ref.Type {
			case "author":
				authorIDs = append(authorIDs, ref.ID)
			case "album":
				albumIDs = append(albumIDs, ref.ID)
			case "track":
				trackIDs = append(trackIDs, ref.ID)
			case "playlist":
				playlistIDs = append(playlistIDs, ref.ID)
			}
		}
		tracks, err := repositories.reads.ListTracksByIDs(ctx, trackIDs)
		if err != nil {
			return err
		}
		for _, item := range tracks {
			albumIDs = append(albumIDs, item.AlbumID)
			authorIDs = append(authorIDs, item.AuthorIDs...)
		}
		albums, err := repositories.reads.ListAlbumsByIDs(ctx, albumIDs)
		if err != nil {
			return err
		}
		authors, err := repositories.reads.ListAuthorsByIDs(ctx, authorIDs)
		if err != nil {
			return err
		}
		playlists, err := repositories.reads.ListPlaylistsByIDs(ctx, playlistIDs)
		if err != nil {
			return err
		}
		preferences, err := repositories.reads.ListPreferencePlaylists(ctx, userID)
		if err != nil {
			return err
		}
		state := newDomainStateView(ctx, s, repositories)
		addTracksToState(state, tracks)
		addAlbumsToState(state, albums)
		addAuthorsToState(state, authors)
		addPlaylistsToState(state, playlists)
		addPlaylistsToState(state, preferences)
		favoriteIDs := state.favoriteTrackSetLocked(userID)
		dislikedIDs := state.dislikedTrackSetLocked(userID)
		items = make([]searchResultItem, 0, len(refs))
		for _, ref := range refs {
			switch ref.Type {
			case "author":
				if item, ok := state.authors[ref.ID]; ok {
					copy := cloneAuthor(item)
					items = append(items, searchResultItem{Type: ref.Type, Author: &copy})
				}
			case "album":
				if item, ok := state.albums[ref.ID]; ok {
					copy := cloneAlbumForRead(item)
					items = append(items, searchResultItem{Type: ref.Type, Album: &copy})
				}
			case "track":
				if item, ok := state.tracks[ref.ID]; ok {
					_, favorite := favoriteIDs[item.ID]
					_, disliked := dislikedIDs[item.ID]
					copy := state.toSearchTrackResponseLocked(item, favorite, disliked)
					items = append(items, searchResultItem{Type: ref.Type, Track: &copy})
				}
			case "playlist":
				if item, ok := state.playlists[ref.ID]; ok {
					copy := buildPlaylistResponse(item)
					if item.UserID != userID {
						copy = publicPlaylistResponse(copy)
					}
					items = append(items, searchResultItem{Type: ref.Type, Playlist: &copy})
				}
			}
		}
		return nil
	})
	if err != nil {
		return paginatedSearchResults{}, fmt.Errorf("search: %w", err)
	}
	page, pageSize, totalPages := paginationMetadata(filter.Page, filter.PageSize, total)
	return paginatedSearchResults{Items: items, Page: page, PageSize: pageSize, TotalItems: total, TotalPages: totalPages}, nil
}

func (s *trackStore) getAuthor(id int64) (author, bool, error) {
	item, ok, err := s.authorRepository.FindByID(context.Background(), id)
	if err != nil {
		return author{}, false, fmt.Errorf("get author: %w", err)
	}
	return item, ok, nil
}

func (s *trackStore) createAuthor(req upsertAuthorRequest) (result author, returnErr error) {
	returnErr = s.withinStateTransaction(stateAuthors, func(state *domainState) error {
		var err error
		result, err = state.createAuthor(req)
		return err
	})
	return
}

func (s *trackStore) updateAuthor(id int64, req upsertAuthorRequest) (result author, found bool, returnErr error) {
	returnErr = s.withinStateTransaction(stateAuthors, func(state *domainState) error {
		var err error
		result, found, err = state.updateAuthor(id, req)
		return err
	})
	return
}

func (s *trackStore) deleteAuthor(id int64) (deleted bool, returnErr error) {
	returnErr = s.withinStateTransaction(stateAuthors|stateTracks, func(state *domainState) error {
		var err error
		deleted, err = state.deleteAuthor(id)
		return err
	})
	return
}

func (s *trackStore) createUser(email, passwordHash string) (result user, returnErr error) {
	returnErr = s.withinStateTransaction(stateUsers|statePlaylists, func(state *domainState) error {
		var err error
		result, err = state.createUser(email, passwordHash)
		return err
	})
	return
}

func (s *trackStore) getUserByEmail(email string) (user, bool, error) {
	item, ok, err := s.userRepository.FindByEmail(context.Background(), email)
	if err != nil {
		return user{}, false, fmt.Errorf("get user by email: %w", err)
	}
	return item, ok, nil
}

func (s *trackStore) getUser(id int64) (user, bool, error) {
	item, ok, err := s.userRepository.FindByID(context.Background(), id)
	if err != nil {
		return user{}, false, fmt.Errorf("get user: %w", err)
	}
	return item, ok, nil
}

func (s *trackStore) createRefreshSession(userID int64, expiresAt time.Time) (result refreshSession, token string, returnErr error) {
	returnErr = s.withinStateTransaction(0, func(state *domainState) error {
		var err error
		result, token, err = state.createRefreshSession(userID, expiresAt)
		return err
	})
	return
}

func (s *trackStore) rotateRefreshSession(rawToken string, expiresAt time.Time) (result user, session refreshSession, token string, returnErr error) {
	returnErr = s.withinStateTransaction(0, func(state *domainState) error {
		var err error
		result, session, token, err = state.rotateRefreshSession(rawToken, expiresAt)
		return err
	})
	return
}

func (s *trackStore) deleteRefreshSession(rawToken string) (deleted bool, returnErr error) {
	returnErr = s.withinStateTransaction(0, func(state *domainState) error {
		var err error
		deleted, err = state.deleteRefreshSession(rawToken)
		return err
	})
	return
}

func (s *trackStore) listTrackResponses(userID int64, filter trackListFilter) (paginatedTracks, error) {
	var responses []trackResponse
	var total int
	err := s.withinReadTransaction(func(ctx context.Context, repositories domainRepositories) error {
		tracks, count, err := repositories.reads.ListTracksPage(ctx, filter)
		if err != nil {
			return err
		}
		total = count
		albumIDs := make([]int64, 0, len(tracks))
		for _, item := range tracks {
			albumIDs = append(albumIDs, item.AlbumID)
		}
		albums, err := repositories.reads.ListAlbumsByIDs(ctx, albumIDs)
		if err != nil {
			return err
		}
		preferences, err := repositories.reads.ListPreferencePlaylists(ctx, userID)
		if err != nil {
			return err
		}
		state := newDomainStateView(ctx, s, repositories)
		addAlbumsToState(state, albums)
		addPlaylistsToState(state, preferences)
		favoriteIDs := state.favoriteTrackSetLocked(userID)
		dislikedIDs := state.dislikedTrackSetLocked(userID)
		responses = make([]trackResponse, 0, len(tracks))
		for _, item := range tracks {
			_, favorite := favoriteIDs[item.ID]
			_, disliked := dislikedIDs[item.ID]
			responses = append(responses, state.toTrackResponseLocked(item, favorite, disliked, true))
		}
		return nil
	})
	if err != nil {
		return paginatedTracks{}, fmt.Errorf("list track responses: %w", err)
	}
	page, pageSize, totalPages := paginationMetadata(filter.Page, filter.PageSize, total)
	return paginatedTracks{Items: responses, Page: page, PageSize: pageSize, TotalItems: total, TotalPages: totalPages}, nil
}

func (s *trackStore) getTrackResponse(trackID, userID int64) (trackResponse, bool, error) {
	var response trackResponse
	var found bool
	err := s.withinReadTransaction(func(ctx context.Context, repositories domainRepositories) error {
		item, ok, err := repositories.catalog.FindTrackByID(ctx, trackID)
		if err != nil || !ok {
			return err
		}
		found = true
		albumItem, albumFound, err := repositories.catalog.FindAlbumByID(ctx, item.AlbumID)
		if err != nil {
			return err
		}
		preferences, err := repositories.reads.ListPreferencePlaylists(ctx, userID)
		if err != nil {
			return err
		}
		state := newDomainStateView(ctx, s, repositories)
		if albumFound {
			addAlbumsToState(state, []album{albumItem})
		}
		addPlaylistsToState(state, preferences)
		_, favorite := state.favoriteTrackSetLocked(userID)[trackID]
		_, disliked := state.dislikedTrackSetLocked(userID)[trackID]
		response = state.toTrackResponseLocked(item, favorite, disliked, true)
		return nil
	})
	if err != nil {
		return trackResponse{}, false, fmt.Errorf("get track response: %w", err)
	}
	return response, found, nil
}

func (s *trackStore) songFileReferenced(fileName string) (bool, int64, error) {
	var references []trackAudioReference
	err := s.withinReadTransaction(func(ctx context.Context, repositories domainRepositories) error {
		var err error
		references, err = repositories.reads.ListTrackAudioReferences(ctx)
		return err
	})
	if err != nil {
		return false, 0, fmt.Errorf("check song reference: %w", err)
	}
	for _, item := range references {
		if trackReferencesSongFile(item.AudioFilePath, fileName) {
			return true, item.ID, nil
		}
	}
	return false, 0, nil
}

func (s *trackStore) referencedSongFiles() (map[string]struct{}, error) {
	var references []trackAudioReference
	err := s.withinReadTransaction(func(ctx context.Context, repositories domainRepositories) error {
		var err error
		references, err = repositories.reads.ListTrackAudioReferences(ctx)
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("list song references: %w", err)
	}
	result := make(map[string]struct{}, len(references))
	for _, item := range references {
		if fileName, ok := extractReferencedSongFileName(item.AudioFilePath); ok {
			result[fileName] = struct{}{}
		}
	}
	return result, nil
}

func (s *trackStore) toTrackResponse(t track, isFavorite, isDisliked, isAvailable bool) (trackResponse, error) {
	var response trackResponse
	err := s.withinReadTransaction(func(ctx context.Context, repositories domainRepositories) error {
		state := newDomainStateView(ctx, s, repositories)
		albumItem, ok, err := repositories.catalog.FindAlbumByID(ctx, t.AlbumID)
		if err != nil {
			return err
		}
		if ok {
			addAlbumsToState(state, []album{albumItem})
		}
		response = state.toTrackResponseLocked(t, isFavorite, isDisliked, isAvailable)
		return nil
	})
	if err != nil {
		return trackResponse{}, fmt.Errorf("build track response: %w", err)
	}
	return response, nil
}

func (s *trackStore) getTrack(trackID int64) (track, bool, error) {
	item, ok, err := s.catalogRepository.FindTrackByID(context.Background(), trackID)
	if err != nil {
		return track{}, false, fmt.Errorf("get import track: %w", err)
	}
	return cloneTrack(item), ok, nil
}

func (s *trackStore) getTrackAlbumOrder(trackID int64) (int, bool, error) {
	var order int
	var found bool
	err := s.withinReadTransaction(func(ctx context.Context, repositories domainRepositories) error {
		item, ok, err := repositories.catalog.FindTrackByID(ctx, trackID)
		if err != nil || !ok {
			return err
		}
		albumItem, ok, err := repositories.catalog.FindAlbumByID(ctx, item.AlbumID)
		if err != nil || !ok {
			return err
		}
		for index, existingTrackID := range albumItem.TrackIDs {
			if existingTrackID == trackID {
				order, found = index, true
				break
			}
		}
		return nil
	})
	if err != nil {
		return 0, false, fmt.Errorf("get track album order: %w", err)
	}
	return order, found, nil
}

func (s *trackStore) findTrackBySourceMetadata(target sourceMetadata) (track, bool, error) {
	return s.findTrackBySourceMetadataContext(context.Background(), target)
}

func (s *trackStore) findTrackBySourceMetadataContext(ctx context.Context, target sourceMetadata) (track, bool, error) {
	provider, ok := target["provider"].(string)
	if !ok || strings.TrimSpace(provider) == "" {
		return track{}, false, nil
	}
	var item track
	var found bool
	err := s.unitOfWork.WithinReadTransaction(ctx, func(repositories domainRepositories) error {
		tracks, err := repositories.reads.ListTracksBySourceProvider(ctx, provider)
		if err != nil {
			return err
		}
		state := newDomainStateView(ctx, s, repositories)
		addTracksToState(state, tracks)
		item, found = state.findTrackBySourceMetadata(target)
		return nil
	})
	if err != nil {
		return track{}, false, fmt.Errorf("find track source metadata: %w", err)
	}
	return item, found, nil
}

func (s *trackStore) createTrackIfSourceAbsent(req upsertTrackRequest, target sourceMetadata, publishAudio func() error) (result track, returnErr error) {
	return s.createTrackIfSourceAbsentContext(context.Background(), req, target, publishAudio)
}

func (s *trackStore) createTrackIfSourceAbsentContext(ctx context.Context, req upsertTrackRequest, target sourceMetadata, publishAudio func() error) (result track, returnErr error) {
	if _, exists, err := s.findTrackBySourceMetadataContext(ctx, target); err != nil {
		return track{}, err
	} else if exists {
		return track{}, errYouTubeCurrentConflict
	}
	if publishAudio != nil {
		if err := publishAudio(); err != nil {
			return track{}, err
		}
	}
	returnErr = s.withinStateTransactionContext(ctx, stateCatalog, func(state *domainState) error {
		var err error
		result, err = state.insertTrackIfSourceAbsent(req, target)
		return err
	})
	return
}

func (s *trackStore) attachTrackImportMetadata(trackID int64, infos []additionalInfo, metadata []sourceMetadata) (result track, found bool, returnErr error) {
	return s.attachTrackImportMetadataContext(context.Background(), trackID, infos, metadata)
}

func (s *trackStore) attachTrackImportMetadataContext(ctx context.Context, trackID int64, infos []additionalInfo, metadata []sourceMetadata) (result track, found bool, returnErr error) {
	returnErr = s.withinStateTransactionContext(ctx, stateCatalog, func(state *domainState) error {
		var err error
		result, found, err = state.attachTrackImportMetadata(trackID, infos, metadata)
		return err
	})
	return
}

func (s *trackStore) attachTrackImportMetadataIfSourceAbsent(trackID int64, infos []additionalInfo, metadata []sourceMetadata, target sourceMetadata) (result track, found bool, returnErr error) {
	return s.attachTrackImportMetadataIfSourceAbsentContext(context.Background(), trackID, infos, metadata, target)
}

func (s *trackStore) attachTrackImportMetadataIfSourceAbsentContext(ctx context.Context, trackID int64, infos []additionalInfo, metadata []sourceMetadata, target sourceMetadata) (result track, found bool, returnErr error) {
	returnErr = s.withinStateTransactionContext(ctx, stateCatalog, func(state *domainState) error {
		var err error
		result, found, err = state.attachTrackImportMetadataIfSourceAbsent(trackID, infos, metadata, target)
		return err
	})
	return
}

func (s *trackStore) youtubeImportSuggestions(item youtubeImportItem) ([]youtubeImportSuggestion, error) {
	var suggestions []youtubeImportSuggestion
	err := s.withinReadState(stateCatalog, func(state *domainState) error {
		suggestions = state.youtubeImportSuggestions(item)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("build YouTube import suggestions: %w", err)
	}
	return suggestions, nil
}
