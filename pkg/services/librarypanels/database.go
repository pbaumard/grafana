package librarypanels

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/grafana/grafana/pkg/api/dtos"
	"github.com/grafana/grafana/pkg/models"
	"github.com/grafana/grafana/pkg/services/sqlstore"
	"github.com/grafana/grafana/pkg/services/sqlstore/migrator"
	"github.com/grafana/grafana/pkg/util"
)

var (
	sqlStatmentLibrayPanelDTOWithMeta = `
SELECT DISTINCT
	lp.id, lp.org_id, lp.folder_id, lp.uid, lp.name, lp.model, lp.created, lp.created_by, lp.updated, lp.updated_by
	, 0 AS can_edit
	, u1.login AS created_by_name
	, u1.email AS created_by_email
	, u2.login AS updated_by_name
	, u2.email AS updated_by_email
	, (SELECT COUNT(dashboard_id) FROM library_panel_dashboard WHERE librarypanel_id = lp.id) AS connected_dashboards
FROM library_panel AS lp
	LEFT JOIN user AS u1 ON lp.created_by = u1.id
	LEFT JOIN user AS u2 ON lp.updated_by = u2.id
`
)

func syncTitleWithName(libraryPanel *LibraryPanel) error {
	var model map[string]interface{}
	if err := json.Unmarshal(libraryPanel.Model, &model); err != nil {
		return err
	}

	model["title"] = libraryPanel.Name
	syncedModel, err := json.Marshal(&model)
	if err != nil {
		return err
	}

	libraryPanel.Model = syncedModel

	return nil
}

// createLibraryPanel adds a Library Panel.
func (lps *LibraryPanelService) createLibraryPanel(c *models.ReqContext, cmd createLibraryPanelCommand) (LibraryPanelDTO, error) {
	libraryPanel := LibraryPanel{
		OrgID:    c.SignedInUser.OrgId,
		FolderID: cmd.FolderID,
		UID:      util.GenerateShortUID(),
		Name:     cmd.Name,
		Model:    cmd.Model,

		Created: time.Now(),
		Updated: time.Now(),

		CreatedBy: c.SignedInUser.UserId,
		UpdatedBy: c.SignedInUser.UserId,
	}

	if err := syncTitleWithName(&libraryPanel); err != nil {
		return LibraryPanelDTO{}, err
	}

	err := lps.SQLStore.WithTransactionalDbSession(c.Context.Req.Context(), func(session *sqlstore.DBSession) error {
		if err := requirePermissionsOnFolder(c.SignedInUser, cmd.FolderID); err != nil {
			return err
		}
		if _, err := session.Insert(&libraryPanel); err != nil {
			if lps.SQLStore.Dialect.IsUniqueConstraintViolation(err) {
				return errLibraryPanelAlreadyExists
			}
			return err
		}
		return nil
	})

	dto := LibraryPanelDTO{
		ID:       libraryPanel.ID,
		OrgID:    libraryPanel.OrgID,
		FolderID: libraryPanel.FolderID,
		UID:      libraryPanel.UID,
		Name:     libraryPanel.Name,
		Model:    libraryPanel.Model,
		Meta: LibraryPanelDTOMeta{
			CanEdit:             true,
			ConnectedDashboards: 0,
			Created:             libraryPanel.Created,
			Updated:             libraryPanel.Updated,
			CreatedBy: LibraryPanelDTOMetaUser{
				ID:        libraryPanel.CreatedBy,
				Name:      c.SignedInUser.Login,
				AvatarUrl: dtos.GetGravatarUrl(c.SignedInUser.Email),
			},
			UpdatedBy: LibraryPanelDTOMetaUser{
				ID:        libraryPanel.UpdatedBy,
				Name:      c.SignedInUser.Login,
				AvatarUrl: dtos.GetGravatarUrl(c.SignedInUser.Email),
			},
		},
	}

	return dto, err
}

func connectDashboard(session *sqlstore.DBSession, dialect migrator.Dialect, user *models.SignedInUser, uid string, dashboardID int64) error {
	panel, err := getLibraryPanel(session, uid, user.OrgId)
	if err != nil {
		return err
	}
	if err := requirePermissionsOnFolder(user, panel.FolderID); err != nil {
		return err
	}

	// TODO add check that dashboard exists

	libraryPanelDashboard := libraryPanelDashboard{
		DashboardID:    dashboardID,
		LibraryPanelID: panel.ID,
		Created:        time.Now(),
		CreatedBy:      user.UserId,
	}
	if _, err := session.Insert(&libraryPanelDashboard); err != nil {
		if dialect.IsUniqueConstraintViolation(err) {
			return nil
		}
		return err
	}
	return nil
}

// connectDashboard adds a connection between a Library Panel and a Dashboard.
func (lps *LibraryPanelService) connectDashboard(c *models.ReqContext, uid string, dashboardID int64) error {
	err := lps.SQLStore.WithTransactionalDbSession(c.Context.Req.Context(), func(session *sqlstore.DBSession) error {
		return connectDashboard(session, lps.SQLStore.Dialect, c.SignedInUser, uid, dashboardID)
	})

	return err
}

// connectLibraryPanelsForDashboard adds connections for all Library Panels in a Dashboard.
func (lps *LibraryPanelService) connectLibraryPanelsForDashboard(c *models.ReqContext, uids []string, dashboardID int64) error {
	err := lps.SQLStore.WithTransactionalDbSession(c.Context.Req.Context(), func(session *sqlstore.DBSession) error {
		_, err := session.Exec("DELETE FROM library_panel_dashboard WHERE dashboard_id=?", dashboardID)
		if err != nil {
			return err
		}
		for _, uid := range uids {
			err := connectDashboard(session, lps.SQLStore.Dialect, c.SignedInUser, uid, dashboardID)
			if err != nil {
				return err
			}
		}
		return nil
	})

	return err
}

// deleteLibraryPanel deletes a Library Panel.
func (lps *LibraryPanelService) deleteLibraryPanel(c *models.ReqContext, uid string) error {
	return lps.SQLStore.WithTransactionalDbSession(c.Context.Req.Context(), func(session *sqlstore.DBSession) error {
		panel, err := getLibraryPanel(session, uid, c.SignedInUser.OrgId)
		if err != nil {
			return err
		}
		if err := requirePermissionsOnFolder(c.SignedInUser, panel.FolderID); err != nil {
			return err
		}
		if _, err := session.Exec("DELETE FROM library_panel_dashboard WHERE librarypanel_id=?", panel.ID); err != nil {
			return err
		}

		result, err := session.Exec("DELETE FROM library_panel WHERE id=?", panel.ID)
		if err != nil {
			return err
		}
		if rowsAffected, err := result.RowsAffected(); err != nil {
			return err
		} else if rowsAffected != 1 {
			return errLibraryPanelNotFound
		}

		return nil
	})
}

// disconnectDashboard deletes a connection between a Library Panel and a Dashboard.
func (lps *LibraryPanelService) disconnectDashboard(c *models.ReqContext, uid string, dashboardID int64) error {
	return lps.SQLStore.WithTransactionalDbSession(c.Context.Req.Context(), func(session *sqlstore.DBSession) error {
		panel, err := getLibraryPanel(session, uid, c.SignedInUser.OrgId)
		if err != nil {
			return err
		}
		if err := requirePermissionsOnFolder(c.SignedInUser, panel.FolderID); err != nil {
			return err
		}

		result, err := session.Exec("DELETE FROM library_panel_dashboard WHERE librarypanel_id=? and dashboard_id=?", panel.ID, dashboardID)
		if err != nil {
			return err
		}

		if rowsAffected, err := result.RowsAffected(); err != nil {
			return err
		} else if rowsAffected != 1 {
			return errLibraryPanelDashboardNotFound
		}

		return nil
	})
}

// disconnectLibraryPanelsForDashboard deletes connections for all Library Panels in a Dashboard.
func (lps *LibraryPanelService) disconnectLibraryPanelsForDashboard(c *models.ReqContext, dashboardID int64, panelCount int64) error {
	return lps.SQLStore.WithTransactionalDbSession(c.Context.Req.Context(), func(session *sqlstore.DBSession) error {
		result, err := session.Exec("DELETE FROM library_panel_dashboard WHERE dashboard_id=?", dashboardID)
		if err != nil {
			return err
		}
		if rowsAffected, err := result.RowsAffected(); err != nil {
			return err
		} else if rowsAffected != panelCount {
			lps.log.Warn("Number of disconnects does not match number of panels", "dashboard", dashboardID, "rowsAffected", rowsAffected, "panelCount", panelCount)
		}

		return nil
	})
}

// deleteLibraryPanelsInFolder deletes all Library Panels for a folder.
func (lps *LibraryPanelService) deleteLibraryPanelsInFolder(c *models.ReqContext, folderUID string) error {
	return lps.SQLStore.WithTransactionalDbSession(c.Context.Req.Context(), func(session *sqlstore.DBSession) error {
		var folderUIDs []struct {
			ID int64 `xorm:"id"`
		}
		err := session.SQL("SELECT id from dashboard WHERE uid=? AND org_id=? AND is_folder=1", folderUID, c.SignedInUser.OrgId).Find(&folderUIDs)
		if err != nil {
			return err
		}
		if len(folderUIDs) != 1 {
			return fmt.Errorf("found %d folders, while expecting at most one", len(folderUIDs))
		}
		folderID := folderUIDs[0].ID

		if err := requirePermissionsOnFolder(c.SignedInUser, folderID); err != nil {
			return err
		}
		var dashIDs []struct {
			DashboardID int64 `xorm:"dashboard_id"`
		}
		sql := "SELECT lpd.dashboard_id FROM library_panel AS lp"
		sql += " INNER JOIN library_panel_dashboard lpd on lp.id = lpd.librarypanel_id"
		sql += " WHERE lp.folder_id=? AND lp.org_id=?"
		err = session.SQL(sql, folderID, c.SignedInUser.OrgId).Find(&dashIDs)
		if err != nil {
			return err
		}
		if len(dashIDs) > 0 {
			return ErrFolderHasConnectedLibraryPanels
		}

		var panelIDs []struct {
			ID int64 `xorm:"id"`
		}
		err = session.SQL("SELECT id from library_panel WHERE folder_id=? AND org_id=?", folderID, c.SignedInUser.OrgId).Find(&panelIDs)
		if err != nil {
			return err
		}
		for _, panelID := range panelIDs {
			_, err := session.Exec("DELETE FROM library_panel_dashboard WHERE librarypanel_id=?", panelID.ID)
			if err != nil {
				return err
			}
		}
		if _, err := session.Exec("DELETE FROM library_panel WHERE folder_id=? AND org_id=?", folderID, c.SignedInUser.OrgId); err != nil {
			return err
		}

		return nil
	})
}

func getLibraryPanel(session *sqlstore.DBSession, uid string, orgID int64) (LibraryPanelWithMeta, error) {
	libraryPanels := make([]LibraryPanelWithMeta, 0)
	sql := sqlStatmentLibrayPanelDTOWithMeta + "WHERE lp.uid=? AND lp.org_id=?"
	sess := session.SQL(sql, uid, orgID)
	err := sess.Find(&libraryPanels)
	if err != nil {
		return LibraryPanelWithMeta{}, err
	}
	if len(libraryPanels) == 0 {
		return LibraryPanelWithMeta{}, errLibraryPanelNotFound
	}
	if len(libraryPanels) > 1 {
		return LibraryPanelWithMeta{}, fmt.Errorf("found %d panels, while expecting at most one", len(libraryPanels))
	}

	return libraryPanels[0], nil
}

// getLibraryPanel gets a Library Panel.
func (lps *LibraryPanelService) getLibraryPanel(c *models.ReqContext, uid string) (LibraryPanelDTO, error) {
	var libraryPanel LibraryPanelWithMeta
	err := lps.SQLStore.WithDbSession(c.Context.Req.Context(), func(session *sqlstore.DBSession) error {
		libraryPanels := make([]LibraryPanelWithMeta, 0)
		builder := sqlstore.SQLBuilder{}
		builder.Write(sqlStatmentLibrayPanelDTOWithMeta)
		builder.Write(` WHERE lp.uid=? AND lp.org_id=? AND lp.folder_id=0`, uid, c.SignedInUser.OrgId)
		builder.Write(" UNION ")
		builder.Write(sqlStatmentLibrayPanelDTOWithMeta)
		builder.Write(" INNER JOIN dashboard AS dashboard on lp.folder_id = dashboard.id AND lp.folder_id <> 0")
		builder.Write(` WHERE lp.uid=? AND lp.org_id=?`, uid, c.SignedInUser.OrgId)
		if c.SignedInUser.OrgRole != models.ROLE_ADMIN {
			builder.WriteDashboardPermissionFilter(c.SignedInUser, models.PERMISSION_VIEW)
		}
		builder.Write(` OR dashboard.id=0`)
		if err := session.SQL(builder.GetSQLString(), builder.GetParams()...).Find(&libraryPanels); err != nil {
			return err
		}
		if len(libraryPanels) == 0 {
			return errLibraryPanelNotFound
		}
		if len(libraryPanels) > 1 {
			return fmt.Errorf("found %d panels, while expecting at most one", len(libraryPanels))
		}

		libraryPanel = libraryPanels[0]

		return nil
	})

	dto := LibraryPanelDTO{
		ID:       libraryPanel.ID,
		OrgID:    libraryPanel.OrgID,
		FolderID: libraryPanel.FolderID,
		UID:      libraryPanel.UID,
		Name:     libraryPanel.Name,
		Model:    libraryPanel.Model,
		Meta: LibraryPanelDTOMeta{
			CanEdit:             true,
			ConnectedDashboards: libraryPanel.ConnectedDashboards,
			Created:             libraryPanel.Created,
			Updated:             libraryPanel.Updated,
			CreatedBy: LibraryPanelDTOMetaUser{
				ID:        libraryPanel.CreatedBy,
				Name:      libraryPanel.CreatedByName,
				AvatarUrl: dtos.GetGravatarUrl(libraryPanel.CreatedByEmail),
			},
			UpdatedBy: LibraryPanelDTOMetaUser{
				ID:        libraryPanel.UpdatedBy,
				Name:      libraryPanel.UpdatedByName,
				AvatarUrl: dtos.GetGravatarUrl(libraryPanel.UpdatedByEmail),
			},
		},
	}

	return dto, err
}

// getAllLibraryPanels gets all library panels.
func (lps *LibraryPanelService) getAllLibraryPanels(c *models.ReqContext, limit int64) ([]LibraryPanelDTO, error) {
	libraryPanels := make([]LibraryPanelWithMeta, 0)
	err := lps.SQLStore.WithDbSession(c.Context.Req.Context(), func(session *sqlstore.DBSession) error {
		builder := sqlstore.SQLBuilder{}
		builder.Write(sqlStatmentLibrayPanelDTOWithMeta)
		builder.Write(` WHERE lp.org_id=? AND lp.folder_id=0`, c.SignedInUser.OrgId)
		builder.Write(" UNION ")
		builder.Write(sqlStatmentLibrayPanelDTOWithMeta)
		builder.Write(" INNER JOIN dashboard AS dashboard on lp.folder_id = dashboard.id AND lp.folder_id<>0")
		builder.Write(` WHERE lp.org_id=?`, c.SignedInUser.OrgId)
		if c.SignedInUser.OrgRole != models.ROLE_ADMIN {
			builder.WriteDashboardPermissionFilter(c.SignedInUser, models.PERMISSION_VIEW)
		}
		if limit == 0 {
			limit = 1000
		}
		builder.Write(lps.SQLStore.Dialect.Limit(limit))
		if err := session.SQL(builder.GetSQLString(), builder.GetParams()...).Find(&libraryPanels); err != nil {
			return err
		}

		return nil
	})

	retDTOs := make([]LibraryPanelDTO, 0)
	for _, panel := range libraryPanels {
		retDTOs = append(retDTOs, LibraryPanelDTO{
			ID:       panel.ID,
			OrgID:    panel.OrgID,
			FolderID: panel.FolderID,
			UID:      panel.UID,
			Name:     panel.Name,
			Model:    panel.Model,
			Meta: LibraryPanelDTOMeta{
				CanEdit:             true,
				ConnectedDashboards: panel.ConnectedDashboards,
				Created:             panel.Created,
				Updated:             panel.Updated,
				CreatedBy: LibraryPanelDTOMetaUser{
					ID:        panel.CreatedBy,
					Name:      panel.CreatedByName,
					AvatarUrl: dtos.GetGravatarUrl(panel.CreatedByEmail),
				},
				UpdatedBy: LibraryPanelDTOMetaUser{
					ID:        panel.UpdatedBy,
					Name:      panel.UpdatedByName,
					AvatarUrl: dtos.GetGravatarUrl(panel.UpdatedByEmail),
				},
			},
		})
	}

	return retDTOs, err
}

// getConnectedDashboards gets all dashboards connected to a Library Panel.
func (lps *LibraryPanelService) getConnectedDashboards(c *models.ReqContext, uid string) ([]int64, error) {
	connectedDashboardIDs := make([]int64, 0)
	err := lps.SQLStore.WithDbSession(c.Context.Req.Context(), func(session *sqlstore.DBSession) error {
		panel, err := getLibraryPanel(session, uid, c.SignedInUser.OrgId)
		if err != nil {
			return err
		}
		var libraryPanelDashboards []libraryPanelDashboard
		builder := sqlstore.SQLBuilder{}
		builder.Write("SELECT lpd.* FROM library_panel_dashboard lpd")
		builder.Write(" INNER JOIN dashboard AS dashboard on lpd.dashboard_id = dashboard.id")
		builder.Write(` WHERE lpd.librarypanel_id=?`, panel.ID)
		if c.SignedInUser.OrgRole != models.ROLE_ADMIN {
			builder.WriteDashboardPermissionFilter(c.SignedInUser, models.PERMISSION_VIEW)
		}
		if err := session.SQL(builder.GetSQLString(), builder.GetParams()...).Find(&libraryPanelDashboards); err != nil {
			return err
		}

		for _, lpd := range libraryPanelDashboards {
			connectedDashboardIDs = append(connectedDashboardIDs, lpd.DashboardID)
		}

		return nil
	})

	return connectedDashboardIDs, err
}

func (lps *LibraryPanelService) getLibraryPanelsForDashboardID(c *models.ReqContext, dashboardID int64) (map[string]LibraryPanelDTO, error) {
	libraryPanelMap := make(map[string]LibraryPanelDTO)
	err := lps.SQLStore.WithDbSession(c.Context.Req.Context(), func(session *sqlstore.DBSession) error {
		var libraryPanels []LibraryPanelWithMeta
		sql := sqlStatmentLibrayPanelDTOWithMeta + "INNER JOIN library_panel_dashboard AS lpd ON lpd.librarypanel_id = lp.id AND lpd.dashboard_id=?"
		sess := session.SQL(sql, dashboardID)
		err := sess.Find(&libraryPanels)
		if err != nil {
			return err
		}

		for _, panel := range libraryPanels {
			libraryPanelMap[panel.UID] = LibraryPanelDTO{
				ID:       panel.ID,
				OrgID:    panel.OrgID,
				FolderID: panel.FolderID,
				UID:      panel.UID,
				Name:     panel.Name,
				Model:    panel.Model,
				Meta: LibraryPanelDTOMeta{
					CanEdit:             panel.CanEdit,
					ConnectedDashboards: panel.ConnectedDashboards,
					Created:             panel.Created,
					Updated:             panel.Updated,
					CreatedBy: LibraryPanelDTOMetaUser{
						ID:        panel.CreatedBy,
						Name:      panel.CreatedByName,
						AvatarUrl: dtos.GetGravatarUrl(panel.CreatedByEmail),
					},
					UpdatedBy: LibraryPanelDTOMetaUser{
						ID:        panel.UpdatedBy,
						Name:      panel.UpdatedByName,
						AvatarUrl: dtos.GetGravatarUrl(panel.UpdatedByEmail),
					},
				},
			}
		}

		return nil
	})

	return libraryPanelMap, err
}

func handleFolderIDPatches(panelToPatch *LibraryPanel, fromFolderID int64, toFolderID int64, user *models.SignedInUser) error {
	// FolderID was not provided in the PATCH request
	if toFolderID == -1 {
		toFolderID = fromFolderID
	}

	// FolderID was provided in the PATCH request
	if toFolderID != -1 && toFolderID != fromFolderID {
		if err := requirePermissionsOnFolder(user, toFolderID); err != nil {
			return err
		}
	}

	// Always check permissions for the folder where library panel resides
	if err := requirePermissionsOnFolder(user, fromFolderID); err != nil {
		return err
	}

	panelToPatch.FolderID = toFolderID

	return nil
}

// patchLibraryPanel updates a Library Panel.
func (lps *LibraryPanelService) patchLibraryPanel(c *models.ReqContext, cmd patchLibraryPanelCommand, uid string) (LibraryPanelDTO, error) {
	var dto LibraryPanelDTO
	err := lps.SQLStore.WithTransactionalDbSession(c.Context.Req.Context(), func(session *sqlstore.DBSession) error {
		panelInDB, err := getLibraryPanel(session, uid, c.SignedInUser.OrgId)
		if err != nil {
			return err
		}

		var libraryPanel = LibraryPanel{
			ID:        panelInDB.ID,
			OrgID:     c.SignedInUser.OrgId,
			FolderID:  cmd.FolderID,
			UID:       uid,
			Name:      cmd.Name,
			Model:     cmd.Model,
			Created:   panelInDB.Created,
			CreatedBy: panelInDB.CreatedBy,
			Updated:   time.Now(),
			UpdatedBy: c.SignedInUser.UserId,
		}

		if cmd.Name == "" {
			libraryPanel.Name = panelInDB.Name
		}
		if cmd.Model == nil {
			libraryPanel.Model = panelInDB.Model
		}
		if err := handleFolderIDPatches(&libraryPanel, panelInDB.FolderID, cmd.FolderID, c.SignedInUser); err != nil {
			return err
		}
		if err := syncTitleWithName(&libraryPanel); err != nil {
			return err
		}
		if rowsAffected, err := session.ID(panelInDB.ID).Update(&libraryPanel); err != nil {
			if lps.SQLStore.Dialect.IsUniqueConstraintViolation(err) {
				return errLibraryPanelAlreadyExists
			}
			return err
		} else if rowsAffected != 1 {
			return errLibraryPanelNotFound
		}

		dto = LibraryPanelDTO{
			ID:       libraryPanel.ID,
			OrgID:    libraryPanel.OrgID,
			FolderID: libraryPanel.FolderID,
			UID:      libraryPanel.UID,
			Name:     libraryPanel.Name,
			Model:    libraryPanel.Model,
			Meta: LibraryPanelDTOMeta{
				CanEdit:             true,
				ConnectedDashboards: panelInDB.ConnectedDashboards,
				Created:             libraryPanel.Created,
				Updated:             libraryPanel.Updated,
				CreatedBy: LibraryPanelDTOMetaUser{
					ID:        panelInDB.CreatedBy,
					Name:      panelInDB.CreatedByName,
					AvatarUrl: dtos.GetGravatarUrl(panelInDB.CreatedByEmail),
				},
				UpdatedBy: LibraryPanelDTOMetaUser{
					ID:        libraryPanel.UpdatedBy,
					Name:      c.SignedInUser.Login,
					AvatarUrl: dtos.GetGravatarUrl(c.SignedInUser.Email),
				},
			},
		}

		return nil
	})

	return dto, err
}