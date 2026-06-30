package user

import (
	"fmt"
	"math"
	"strconv"
	"time"

	"gorm.io/gorm"
	"xraytool/internal/appconfig"
	"xraytool/internal/database"
	"xraytool/internal/generate"
	"xraytool/internal/slave"
	"xraytool/internal/xrayapi"
	"xraytool/internal/xrayconfig"
)

type Service struct {
	db  *gorm.DB
	cfg *appconfig.Config
}

func NewService(db *gorm.DB, cfg *appconfig.Config) *Service {
	return &Service{db: db, cfg: cfg}
}

type CreateUserRequest struct {
	Email   string
	Name    string
	UUID    string
	Subfile string
	Expire  string
	Auth    string
	Limit   *float64
	Legacy  bool
	SkipDB  bool // Used by slaves via internal sync
}

type CreateUserResponse struct {
	Email   string
	UUID    string
	Subfile string
	Expire  string
	Auth    string
	Link    string
}

func (s *Service) CreateUser(req CreateUserRequest) (*CreateUserResponse, error) {
	email := req.Email
	if email == "" {
		email = req.Name
	}
	if email == "" {
		return nil, fmt.Errorf("email is required")
	}

	uuid := req.UUID
	if uuid == "" {
		u, err := generate.UUID()
		if err != nil {
			return nil, fmt.Errorf("generating UUID: %v", err)
		}
		uuid = u
	}

	subfile := req.Subfile
	if subfile == "" {
		subfile = generate.Subfile()
	}

	expireVal := req.Expire
	if expireVal == "" {
		// default +30 days
		expireVal = time.Now().AddDate(0, 1, 0).Format("02-01-2006")
	}

	auth := req.Auth
	if auth == "" {
		auth = generate.Secret(32)
	}

	xrayCfg, err := xrayconfig.Read(s.cfg.Paths.XrayConfig)
	if err != nil {
		return nil, fmt.Errorf("reading xray config: %v", err)
	}
	exists, existsErr := xrayconfig.UserExists(xrayCfg, email)
	if existsErr != nil {
		return nil, fmt.Errorf("checking user existence: %v", existsErr)
	}
	if exists {
		return nil, fmt.Errorf("user already exists")
	}

	params := xrayconfig.ClientParams{
		Email:   email,
		UUID:    uuid,
		Auth:    auth,
		Subfile: subfile,
		Expire:  expireVal,
		Limit:   req.Limit,
	}

	clientsPayload, err := xrayconfig.BuildForAllInbounds(xrayCfg, params)
	if err != nil {
		return nil, fmt.Errorf("building client payload: %v", err)
	}
	if len(clientsPayload) == 0 {
		return nil, fmt.Errorf("no client inbounds found in xray config")
	}

	if err := xrayconfig.AddUserToInbounds(xrayCfg, clientsPayload); err != nil {
		return nil, fmt.Errorf("updating xray config: %v", err)
	}

	if err := xrayconfig.Write(s.cfg.Paths.XrayConfig, xrayCfg); err != nil {
		return nil, fmt.Errorf("writing xray config: %v", err)
	}

	if !req.Legacy {
		apiClient := xrayapi.NewGRPCClient(s.cfg.Xray.APIAddr)
		if err := apiClient.AddUser(clientsPayload, s.cfg.Paths.XrayConfig); err != nil {
			// Don't fail the whole request, just warn/ignore since config is saved
		}
	}

	if !req.SkipDB && s.db != nil && database.IsReady() {
		limitInt := 3
		if req.Limit != nil {
			limitInt = int(*req.Limit)
		}

		var sub database.Subscription
		if err := s.db.Where("email = ?", email).First(&sub).Error; err != nil {
			var endsAt *time.Time
			if t, err := time.Parse("02-01-2006", expireVal); err == nil {
				endsAt = &t
			}
			userID, _ := generate.UUID()
			if userID == "" {
				userID = uuid
			}
			subID, _ := generate.UUID()
			if subID == "" {
				subID = uuid + "-sub"
			}

			s.db.Create(&database.User{
				ID:        userID,
				Username:  email,
				RefCode:   "ref_" + generate.Secret(8),
				CreatedAt: time.Now(),
			})
			s.db.Create(&database.Subscription{
				ID:         subID,
				UserID:     userID,
				Email:      email,
				XrayUUID:   uuid,
				Status:     "active",
				MaxDevices: limitInt,
				EndsAt:     endsAt,
				Metadata:   map[string]interface{}{"subfile": subfile},
				CreatedAt:  time.Now(),
				UpdatedAt:  time.Now(),
			})
		} else {
			if sub.Metadata == nil {
				sub.Metadata = make(map[string]interface{})
			}
			if sub.XrayUUID != uuid {
				sub.XrayUUID = uuid
			}
			if sf, ok := sub.Metadata["subfile"].(string); !ok || sf != subfile {
				sub.Metadata["subfile"] = subfile
			}
			s.db.Save(&sub)
		}
	}

	// Propagate to slaves
	if s.cfg.IsMaster() {
		go func() {
			slaveParams := map[string]string{
				"email":   email,
				"uuid":    uuid,
				"subfile": subfile,
				"expire":  expireVal,
				"auth":    auth,
			}
			if req.Limit != nil {
				slaveParams["limit"] = strconv.FormatFloat(*req.Limit, 'f', 0, 64)
			}
			if req.Legacy {
				slaveParams["legacy"] = "true"
			}

			c := slave.NewClient(
				s.cfg.SlaveAPI.ConnectTimeout,
				s.cfg.SlaveAPI.RequestTimeout,
				s.cfg.SlaveAPI.RemotePath,
			)
			reg := slave.NewRegistry(s.cfg.SlaveServers, c)
			reg.PropagateAll("newuser", slaveParams)
		}()
	}

	// subfile logic for link
	id := subfile
	if len(id) > 5 && id[len(id)-4:] == ".txt" {
		id = id[:len(id)-4]
	}
	link := s.GenerateShareLink("", id)

	return &CreateUserResponse{
		Email:   email,
		UUID:    uuid,
		Subfile: subfile,
		Expire:  expireVal,
		Auth:    auth,
		Link:    link,
	}, nil
}

type UnlimitUserRequest struct {
	Email   string
	Name    string
	UUID    string
	Subfile string
	Expire  string
	Auth    string
	Limit   *float64
	Legacy  bool
	SkipDB  bool
}

func (s *Service) UnlimitUser(req UnlimitUserRequest) (*CreateUserResponse, error) {
	email := req.Email
	if email == "" {
		email = req.Name
	}
	if email == "" {
		return nil, fmt.Errorf("email is required")
	}

	xrayCfg, err := xrayconfig.Read(s.cfg.Paths.XrayConfig)
	if err != nil {
		return nil, fmt.Errorf("reading xray config: %v", err)
	}

	isActive, _ := xrayconfig.UserExists(xrayCfg, email)

	uuid := req.UUID
	subfile := req.Subfile
	expireVal := req.Expire
	auth := req.Auth

	if !req.SkipDB && s.db != nil && database.IsReady() {
		s.db.Where("email = ?", email).Delete(&database.AntifraudBan{})
	}

	if uuid == "" && isActive {
		if c, _ := xrayconfig.FindUser(xrayCfg, email); c != nil {
			uuid = c.GetString("id")
			if subfile == "" {
				subfile = c.GetString("subfile")
			}
			if expireVal == "" {
				expireVal = c.GetString("expire")
			}
			if auth == "" {
				auth = c.GetString("auth")
			}
		}
	}

	if uuid == "" {
		uuid, _ = generate.UUID()
	}
	if subfile == "" {
		subfile = generate.Subfile()
	}
	if expireVal == "" {
		expireVal = time.Now().AddDate(0, 1, 0).Format("02-01-2006")
	}
	if auth == "" {
		auth = generate.Secret(32)
	}

	params := xrayconfig.ClientParams{
		Email:   email,
		UUID:    uuid,
		Auth:    auth,
		Subfile: subfile,
		Expire:  expireVal,
		Limit:   req.Limit,
	}

	clientsPayload, err := xrayconfig.BuildForAllInbounds(xrayCfg, params)
	if err != nil {
		return nil, fmt.Errorf("building client payload: %v", err)
	}
	if len(clientsPayload) == 0 {
		return nil, fmt.Errorf("no client inbounds found in xray config")
	}

	if err := xrayconfig.AddUserToInbounds(xrayCfg, clientsPayload); err != nil {
		return nil, fmt.Errorf("updating xray config: %v", err)
	}

	if err := xrayconfig.Write(s.cfg.Paths.XrayConfig, xrayCfg); err != nil {
		return nil, fmt.Errorf("writing xray config: %v", err)
	}

	if !req.Legacy {
		apiClient := xrayapi.NewGRPCClient(s.cfg.Xray.APIAddr)
		_ = apiClient.AddUser(clientsPayload, s.cfg.Paths.XrayConfig)
	}

	if !req.SkipDB && s.db != nil && database.IsReady() {
		s.setStatus(email, "active")
	}

	if s.cfg.IsMaster() {
		go func() {
			slaveParams := map[string]string{
				"email":   email,
				"uuid":    uuid,
				"subfile": subfile,
				"expire":  expireVal,
				"auth":    auth,
			}
			if req.Limit != nil {
				slaveParams["limit"] = strconv.FormatFloat(*req.Limit, 'f', 0, 64)
			}
			if req.Legacy {
				slaveParams["legacy"] = "true"
			}
			c := slave.NewClient(
				s.cfg.SlaveAPI.ConnectTimeout,
				s.cfg.SlaveAPI.RequestTimeout,
				s.cfg.SlaveAPI.RemotePath,
			)
			reg := slave.NewRegistry(s.cfg.SlaveServers, c)
			reg.PropagateAll("unlimit", slaveParams)
		}()
	}

	id := subfile
	if len(id) > 5 && id[len(id)-4:] == ".txt" {
		id = id[:len(id)-4]
	}
	link := s.GenerateShareLink("", id)

	return &CreateUserResponse{
		Email:   email,
		UUID:    uuid,
		Subfile: subfile,
		Expire:  expireVal,
		Auth:    auth,
		Link:    link,
	}, nil
}

type SetExpireRequest struct {
	Email  string
	Name   string
	Expire string
	SkipDB bool
}

func (s *Service) SetExpire(req SetExpireRequest) error {
	email := req.Email
	if email == "" {
		email = req.Name
	}
	if email == "" || req.Expire == "" {
		return fmt.Errorf("email and expire are required")
	}

	updatedActive := false
	if err := xrayconfig.Modify(s.cfg.Paths.XrayConfig, func(c xrayconfig.RawConfig) error {
		exists, _ := xrayconfig.UserExists(c, email)
		if !exists {
			return nil
		}
		updatedActive = true
		return xrayconfig.UpdateStringField(c, email, "expire", req.Expire)
	}); err != nil {
		return fmt.Errorf("updating expire: %v", err)
	}

	if !updatedActive {
		return fmt.Errorf("user %q not found in xray config", email)
	}

	if !req.SkipDB && s.db != nil && database.IsReady() {
		s.setExpireDB(email, req.Expire)
	}

	if s.cfg.IsMaster() {
		go func() {
			c := slave.NewClient(s.cfg.SlaveAPI.ConnectTimeout, s.cfg.SlaveAPI.RequestTimeout, s.cfg.SlaveAPI.RemotePath)
			reg := slave.NewRegistry(s.cfg.SlaveServers, c)
			reg.PropagateAll("setexpire", map[string]string{"email": email, "expire": req.Expire})
		}()
	}
	return nil
}

type UpdateLimitRequest struct {
	Email  string
	Name   string
	Limit  *float64
	SkipDB bool
}

func (s *Service) UpdateLimit(req UpdateLimitRequest) error {
	email := req.Email
	if email == "" {
		email = req.Name
	}
	if email == "" || req.Limit == nil {
		return fmt.Errorf("email and limit are required")
	}

	updatedActive := false
	if err := xrayconfig.Modify(s.cfg.Paths.XrayConfig, func(c xrayconfig.RawConfig) error {
		exists, _ := xrayconfig.UserExists(c, email)
		if !exists {
			return nil
		}
		updatedActive = true
		return xrayconfig.UpdateNumberField(c, email, "limit", *req.Limit)
	}); err != nil {
		return fmt.Errorf("updating active config: %v", err)
	}

	if !updatedActive {
		return fmt.Errorf("user %q not found in xray config", email)
	}

	if !req.SkipDB && s.db != nil && database.IsReady() {
		s.setLimitDB(email, int(*req.Limit))
	}

	if s.cfg.IsMaster() {
		go func() {
			c := slave.NewClient(s.cfg.SlaveAPI.ConnectTimeout, s.cfg.SlaveAPI.RequestTimeout, s.cfg.SlaveAPI.RemotePath)
			reg := slave.NewRegistry(s.cfg.SlaveServers, c)
			reg.PropagateAll("setlimit", map[string]string{
				"email": email,
				"limit": strconv.FormatFloat(*req.Limit, 'f', 0, 64),
			})
		}()
	}
	return nil
}

type ModifyUserRequest struct {
	Email  string
	Name   string
	Action string // "limit" or "rm"
	Legacy bool
	SkipDB bool
}

func (s *Service) BlockOrRemoveUser(req ModifyUserRequest) error {
	email := req.Email
	if email == "" {
		email = req.Name
	}
	if email == "" {
		return fmt.Errorf("email is required")
	}

	xrayCfg, err := xrayconfig.Read(s.cfg.Paths.XrayConfig)
	if err != nil {
		return fmt.Errorf("reading xray config: %v", err)
	}

	client, err := xrayconfig.FindUser(xrayCfg, email)
	if err != nil || client == nil {
		if req.Action == "rm" && !req.SkipDB && s.db != nil && database.IsReady() {
			s.setStatus(email, "inactive")
			s.propagateBlockOrRemove(email, req.Action, req.Legacy)
			return nil
		}
		return fmt.Errorf("user is already limited/blocked or not in xray config")
	}

	if !req.Legacy {
		tags, _ := xrayconfig.InboundTagsForUser(xrayCfg, email)
		apiClient := xrayapi.NewGRPCClient(s.cfg.Xray.APIAddr)
		_ = apiClient.RemoveUser(email, tags)
	}

	if err := xrayconfig.RemoveUserFromAllInbounds(xrayCfg, email); err != nil {
		return fmt.Errorf("removing from xray config: %v", err)
	}
	if err := xrayconfig.Write(s.cfg.Paths.XrayConfig, xrayCfg); err != nil {
		return fmt.Errorf("writing xray config: %v", err)
	}

	if !req.SkipDB && s.db != nil && database.IsReady() {
		status := "inactive"
		if req.Action == "limit" {
			status = "blocked"
		}
		s.setStatus(email, status)
	}

	s.propagateBlockOrRemove(email, req.Action, req.Legacy)
	return nil
}

func (s *Service) propagateBlockOrRemove(email, action string, legacy bool) {
	if !s.cfg.IsMaster() {
		return
	}
	go func() {
		slaveCmd := action
		if action == "rm" {
			slaveCmd = "rmuser"
		}
		slaveParams := map[string]string{"email": email}
		if legacy {
			slaveParams["legacy"] = "true"
		}
		c := slave.NewClient(s.cfg.SlaveAPI.ConnectTimeout, s.cfg.SlaveAPI.RequestTimeout, s.cfg.SlaveAPI.RemotePath)
		reg := slave.NewRegistry(s.cfg.SlaveServers, c)
		reg.PropagateAll(slaveCmd, slaveParams)
	}()
}

func (s *Service) setStatus(email, status string) {
	var sub database.Subscription
	if err := s.db.Where("email = ?", email).Order("created_at desc").First(&sub).Error; err == nil {
		s.db.Model(&sub).Updates(map[string]interface{}{
			"status":     status,
			"updated_at": time.Now(),
		})
	}
}

func (s *Service) setExpireDB(email string, expireVal string) {
	var sub database.Subscription
	if err := s.db.Where("email = ?", email).Order("created_at desc").First(&sub).Error; err == nil {
		updates := map[string]interface{}{
			"updated_at": time.Now(),
		}
		// Assuming format 02-01-2006
		if t, err := time.Parse("02-01-2006", expireVal); err == nil {
			updates["ends_at"] = t
		}
		s.db.Model(&sub).Updates(updates)
	}
}

func (s *Service) setLimitDB(email string, limit int) {
	var sub database.Subscription
	if err := s.db.Where("email = ?", email).Order("created_at desc").First(&sub).Error; err == nil {
		s.db.Model(&sub).Updates(map[string]interface{}{
			"max_devices": limit,
			"updated_at":  time.Now(),
		})
	}
}

// ParseLimit parses a string into a *float64 limit, validating bounds.
func ParseLimit(s string) (*float64, error) {
	if s == "" {
		return nil, nil
	}
	var v float64
	if _, err := fmt.Sscanf(s, "%f", &v); err != nil {
		return nil, fmt.Errorf("invalid limit %q: %w", s, err)
	}
	if v < 1 || v != math.Trunc(v) || v > 10000 {
		return nil, fmt.Errorf("limit must be a positive integer between 1 and 10000 (got %v)", v)
	}
	return &v, nil
}

// GenerateShareLink creates the canonical subscription URL for a client.
// If host is empty, it falls back to the configured server domain.
func (s *Service) GenerateShareLink(host, id string) string {
	domain := host
	if domain == "" {
		domain = s.cfg.Server.Domain
	}
	return fmt.Sprintf("https://%s/client?id=%s", domain, id)
}
