package main

import (
	"context"
	"fmt"
	"log"
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
	state := &domainState{
		trackStore: store, ctx: ctx, repositories: repositories,
		tracks: make(map[int64]track), albums: make(map[int64]album), authors: make(map[int64]author),
		users: make(map[int64]user), usersByEmail: make(map[string]int64),
		playlists: make(map[int64]playlist), lyricsByTrack: make(map[int64]lyrics),
	}
	if domains&stateTracks != 0 {
		items, err := repositories.catalog.ListTracks(ctx)
		if err != nil {
			return nil, err
		}
		for _, item := range items {
			state.tracks[item.ID] = item
		}
	}
	if domains&stateAlbums != 0 {
		items, err := repositories.catalog.ListAlbums(ctx)
		if err != nil {
			return nil, err
		}
		for _, item := range items {
			state.albums[item.ID] = item
		}
	}
	if domains&stateAuthors != 0 {
		items, err := repositories.authors.List(ctx)
		if err != nil {
			return nil, err
		}
		for _, item := range items {
			state.authors[item.ID] = item
		}
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
		for _, item := range items {
			state.playlists[item.ID] = item
		}
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

func (s *trackStore) readState(domains stateDomain) (*domainState, error) {
	return s.readStateContext(context.Background(), domains)
}

func (s *trackStore) readStateContext(ctx context.Context, domains stateDomain) (*domainState, error) {
	return loadDomainState(ctx, s, newDomainRepositories(s.db), domains)
}

func (s *trackStore) logReadFailure(operation string, err error) {
	log.Printf("%s: %v", operation, err)
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

func (s *trackStore) list() []track {
	state, err := s.readState(stateTracks)
	if err != nil {
		s.logReadFailure("list tracks", err)
		return nil
	}
	return state.list()
}

func (s *trackStore) listAlbums(filter albumListFilter) paginatedAlbums {
	state, err := s.readState(stateAlbums)
	if err != nil {
		s.logReadFailure("list albums", err)
		return paginatedAlbums{}
	}
	return state.listAlbums(filter)
}

func (s *trackStore) getAlbum(id int64) (album, bool) {
	item, ok, err := s.catalogRepository.FindAlbumByID(context.Background(), id)
	if err != nil {
		s.logReadFailure("get album", err)
		return album{}, false
	}
	return item, ok
}

func (s *trackStore) getAlbumTracks(id, userID int64) ([]trackResponse, bool) {
	state, err := s.readState(stateTracks | stateAlbums | statePlaylists)
	if err != nil {
		s.logReadFailure("get album tracks", err)
		return nil, false
	}
	return state.getAlbumTracks(id, userID)
}

func (s *trackStore) get(id int64) (track, bool) {
	item, ok, err := s.catalogRepository.FindTrackByID(context.Background(), id)
	if err != nil {
		s.logReadFailure("get track", err)
		return track{}, false
	}
	return item, ok
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

func (s *trackStore) listPlaylists(userID int64, filter playlistListFilter) paginatedPlaylists {
	state, err := s.readState(statePlaylists)
	if err != nil {
		s.logReadFailure("list playlists", err)
		return paginatedPlaylists{}
	}
	return state.listPlaylists(userID, filter)
}

func (s *trackStore) getPlaylist(userID, playlistID int64) (playlistResponse, bool) {
	state, err := s.readState(statePlaylists)
	if err != nil {
		s.logReadFailure("get playlist", err)
		return playlistResponse{}, false
	}
	return state.getPlaylist(userID, playlistID)
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
	state, err := s.readState(statePlaylists)
	if err != nil {
		return false, err
	}
	return state.validatePlaylistCoverUploadTarget(userID, playlistID)
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

func (s *trackStore) playlistReadState(operation string) (*domainState, bool) {
	state, err := s.readState(stateTracks | stateAlbums | statePlaylists)
	if err != nil {
		s.logReadFailure(operation, err)
		return nil, false
	}
	return state, true
}

func (s *trackStore) getPlaylistTracks(userID, playlistID, page, pageSize int64) (paginatedTracks, bool) {
	state, ok := s.playlistReadState("get playlist tracks")
	if !ok {
		return paginatedTracks{}, false
	}
	return state.getPlaylistTracks(userID, playlistID, page, pageSize)
}

func (s *trackStore) getPublicPlaylist(playlistID int64) (playlistResponse, bool) {
	state, err := s.readState(statePlaylists)
	if err != nil {
		s.logReadFailure("get public playlist", err)
		return playlistResponse{}, false
	}
	return state.getPublicPlaylist(playlistID)
}

func (s *trackStore) getPublicPlaylistTracks(playlistID, userID, page, pageSize int64) (paginatedTracks, bool) {
	state, ok := s.playlistReadState("get public playlist tracks")
	if !ok {
		return paginatedTracks{}, false
	}
	return state.getPublicPlaylistTracks(playlistID, userID, page, pageSize)
}

func (s *trackStore) getSharedPlaylist(token string) (playlistResponse, bool) {
	state, err := s.readState(statePlaylists)
	if err != nil {
		s.logReadFailure("get shared playlist", err)
		return playlistResponse{}, false
	}
	return state.getSharedPlaylist(token)
}

func (s *trackStore) getSharedPlaylistTracks(token string, userID, page, pageSize int64) (paginatedTracks, bool) {
	state, ok := s.playlistReadState("get shared playlist tracks")
	if !ok {
		return paginatedTracks{}, false
	}
	return state.getSharedPlaylistTracks(token, userID, page, pageSize)
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
	state, err := s.readState(stateCatalog | stateUsers | statePlaylists)
	if err != nil {
		return autoplayNextResponse{}, err
	}
	return state.nextAutoplayTracks(userID, req)
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
	state, err := s.readState(stateAuthors)
	if err != nil {
		return nil, err
	}
	return state.listAuthors(filter)
}

func (s *trackStore) search(userID int64, filter searchListFilter) paginatedSearchResults {
	state, err := s.readState(stateCatalog | statePlaylists)
	if err != nil {
		s.logReadFailure("search", err)
		return paginatedSearchResults{}
	}
	return state.search(userID, filter)
}

func (s *trackStore) getAuthor(id int64) (author, bool) {
	item, ok, err := s.authorRepository.FindByID(context.Background(), id)
	if err != nil {
		s.logReadFailure("get author", err)
		return author{}, false
	}
	return item, ok
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

func (s *trackStore) getUserByEmail(email string) (user, bool) {
	item, ok, err := s.userRepository.FindByEmail(context.Background(), email)
	if err != nil {
		s.logReadFailure("get user by email", err)
		return user{}, false
	}
	return item, ok
}

func (s *trackStore) getUser(id int64) (user, bool) {
	item, ok, err := s.userRepository.FindByID(context.Background(), id)
	if err != nil {
		s.logReadFailure("get user", err)
		return user{}, false
	}
	return item, ok
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

func (s *trackStore) listTrackResponses(userID int64, filter trackListFilter) paginatedTracks {
	state, err := s.readState(stateTracks | stateAlbums | statePlaylists)
	if err != nil {
		s.logReadFailure("list track responses", err)
		return paginatedTracks{}
	}
	return state.listTrackResponses(userID, filter)
}

func (s *trackStore) getTrackResponse(trackID, userID int64) (trackResponse, bool) {
	state, err := s.readState(stateTracks | stateAlbums | statePlaylists)
	if err != nil {
		s.logReadFailure("get track response", err)
		return trackResponse{}, false
	}
	return state.getTrackResponse(trackID, userID)
}

func (s *trackStore) songFileReferenced(fileName string) (bool, int64) {
	state, err := s.readState(stateTracks)
	if err != nil {
		s.logReadFailure("check song reference", err)
		return false, 0
	}
	return state.songFileReferenced(fileName)
}

func (s *trackStore) referencedSongFiles() map[string]struct{} {
	state, err := s.readState(stateTracks)
	if err != nil {
		s.logReadFailure("list song references", err)
		return map[string]struct{}{}
	}
	return state.referencedSongFiles()
}

func (s *trackStore) toTrackResponse(t track, isFavorite, isDisliked, isAvailable bool) trackResponse {
	state, err := s.readState(stateAlbums)
	if err != nil {
		s.logReadFailure("build track response", err)
		return toTrackResponse(t, isFavorite, isAvailable)
	}
	return state.toTrackResponse(t, isFavorite, isDisliked, isAvailable)
}

func (s *trackStore) getTrack(trackID int64) (track, bool) {
	item, ok, err := s.catalogRepository.FindTrackByID(context.Background(), trackID)
	if err != nil {
		s.logReadFailure("get import track", err)
		return track{}, false
	}
	return cloneTrack(item), ok
}

func (s *trackStore) getTrackAlbumOrder(trackID int64) (int, bool) {
	state, err := s.readState(stateTracks | stateAlbums)
	if err != nil {
		s.logReadFailure("get track album order", err)
		return 0, false
	}
	return state.getTrackAlbumOrder(trackID)
}

func (s *trackStore) findTrackBySourceMetadata(target sourceMetadata) (track, bool) {
	return s.findTrackBySourceMetadataContext(context.Background(), target)
}

func (s *trackStore) findTrackBySourceMetadataContext(ctx context.Context, target sourceMetadata) (track, bool) {
	state, err := s.readStateContext(ctx, stateTracks)
	if err != nil {
		s.logReadFailure("find track source metadata", err)
		return track{}, false
	}
	return state.findTrackBySourceMetadata(target)
}

func (s *trackStore) createTrackIfSourceAbsent(req upsertTrackRequest, target sourceMetadata, publishAudio func() error) (result track, returnErr error) {
	return s.createTrackIfSourceAbsentContext(context.Background(), req, target, publishAudio)
}

func (s *trackStore) createTrackIfSourceAbsentContext(ctx context.Context, req upsertTrackRequest, target sourceMetadata, publishAudio func() error) (result track, returnErr error) {
	if _, exists := s.findTrackBySourceMetadataContext(ctx, target); exists {
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

func (s *trackStore) youtubeImportSuggestions(item youtubeImportItem) []youtubeImportSuggestion {
	state, err := s.readState(stateCatalog)
	if err != nil {
		s.logReadFailure("build YouTube import suggestions", err)
		return nil
	}
	return state.youtubeImportSuggestions(item)
}
