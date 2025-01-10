package models

import (
	"log"
	"sort"
	"strings"
	"time"

	"github.com/ccfos/nightingale/v6/pkg/ctx"
	"github.com/ccfos/nightingale/v6/pkg/poster"
	"golang.org/x/exp/slices"

	"github.com/pkg/errors"
	"github.com/toolkits/pkg/container/set"

	"gorm.io/gorm"
)

type TargetDeleteHookFunc func(ctx *ctx.Context, idents []string) error

type Target struct {
	Id           int64             `json:"id" gorm:"primaryKey"`
	GroupId      int64             `json:"group_id"`
	GroupObjs    []*BusiGroup      `json:"group_objs" gorm:"-"`
	Ident        string            `json:"ident"`
	Note         string            `json:"note"`
	Tags         string            `json:"-"` // user tags
	TagsJSON     []string          `json:"tags" gorm:"-"`
	TagsMap      map[string]string `json:"tags_maps" gorm:"-"` // internal use, append tags to series
	UpdateAt     int64             `json:"update_at"`
	HostIp       string            `json:"host_ip"` //ipv4，do not needs range select
	AgentVersion string            `json:"agent_version"`
	EngineName   string            `json:"engine_name"`
	OS           string            `json:"os" gorm:"column:os"`
	HostTags     []string          `json:"host_tags" gorm:"serializer:json"`

	UnixTime   int64    `json:"unixtime" gorm:"-"`
	Offset     int64    `json:"offset" gorm:"-"`
	TargetUp   float64  `json:"target_up" gorm:"-"`
	MemUtil    float64  `json:"mem_util" gorm:"-"`
	CpuNum     int      `json:"cpu_num" gorm:"-"`
	CpuUtil    float64  `json:"cpu_util" gorm:"-"`
	Arch       string   `json:"arch" gorm:"-"`
	RemoteAddr string   `json:"remote_addr" gorm:"-"`
	GroupIds   []int64  `json:"group_ids" gorm:"-"`
	GroupNames []string `json:"group_names" gorm:"-"`
}

func (t *Target) TableName() string {
	return "target"
}

func (t *Target) FillGroup(ctx *ctx.Context, cache map[int64]*BusiGroup) error {
	var err error
	if len(t.GroupIds) == 0 {
		t.GroupIds, err = TargetGroupIdsGetByIdent(ctx, t.Ident)
		if err != nil {
			return errors.WithMessage(err, "failed to get target gids")
		}
		t.GroupObjs = make([]*BusiGroup, 0, len(t.GroupIds))
	}

	for _, gid := range t.GroupIds {
		bg, has := cache[gid]
		if has && bg != nil {
			t.GroupObjs = append(t.GroupObjs, bg)
			continue
		}

		bg, err := BusiGroupGetById(ctx, gid)
		if err != nil {
			return errors.WithMessage(err, "failed to get busi group")
		}

		if bg == nil {
			continue
		}

		t.GroupObjs = append(t.GroupObjs, bg)
		cache[gid] = bg
	}

	return nil
}

func (t *Target) MatchGroupId(gid ...int64) bool {
	for _, tgId := range t.GroupIds {
		for _, id := range gid {
			if tgId == id {
				return true
			}
		}
	}
	return false
}

func (t *Target) AfterFind(tx *gorm.DB) (err error) {
	delta := time.Now().Unix() - t.UpdateAt
	if delta < 60 {
		t.TargetUp = 2
	} else if delta < 180 {
		t.TargetUp = 1
	}
	t.FillTagsMap()
	return
}

func TargetStatistics(ctx *ctx.Context) (*Statistics, error) {
	if !ctx.IsCenter {
		s, err := poster.GetByUrls[*Statistics](ctx, "/v1/n9e/statistic?name=target")
		return s, err
	}

	var stats []*Statistics
	err := DB(ctx).Model(&Target{}).Select("count(*) as total", "max(update_at) as last_updated").Find(&stats).Error
	if err != nil {
		return nil, err
	}

	return stats[0], nil
}

func TargetDel(ctx *ctx.Context, idents []string, deleteHook TargetDeleteHookFunc) error {
	if len(idents) == 0 {
		panic("idents empty")
	}

	return DB(ctx).Transaction(func(tx *gorm.DB) error {
		txErr := tx.Where("ident in ?", idents).Delete(new(Target)).Error
		if txErr != nil {
			return txErr
		}
		txErr = deleteHook(ctx, idents)
		if txErr != nil {
			return txErr
		}
		txErr = TargetDeleteBgids(ctx, idents)
		if txErr != nil {
			return txErr
		}
		return nil
	})
}

type BuildTargetWhereOption func(session *gorm.DB) *gorm.DB

func BuildTargetWhereWithBgids(bgids []int64) BuildTargetWhereOption {
	return func(session *gorm.DB) *gorm.DB {
		if len(bgids) == 1 && bgids[0] == 0 {
			session = session.Joins("left join target_busi_group on target.ident = " +
				"target_busi_group.target_ident").Where("target_busi_group.target_ident is null")
		} else if len(bgids) > 0 {
			if slices.Contains(bgids, 0) {
				session = session.Joins("left join target_busi_group on target.ident = target_busi_group.target_ident").
					Where("target_busi_group.target_ident is null OR target_busi_group.group_id in (?)", bgids)
			} else {
				session = session.Joins("join target_busi_group on target.ident = "+
					"target_busi_group.target_ident").Where("target_busi_group.group_id in (?)", bgids)
			}
		}
		return session
	}
}

func BuildTargetWhereWithDsIds(dsIds []int64) BuildTargetWhereOption {
	return func(session *gorm.DB) *gorm.DB {
		if len(dsIds) > 0 {
			session = session.Where("datasource_id in (?)", dsIds)
		}
		return session
	}
}

func BuildTargetWhereWithHosts(hosts []string) BuildTargetWhereOption {
	return func(session *gorm.DB) *gorm.DB {
		if len(hosts) > 0 {
			session = session.Where("ident in (?) or host_ip in (?)", hosts, hosts)
		}
		return session
	}
}

func BuildTargetWhereWithQuery(query string) BuildTargetWhereOption {
	return func(session *gorm.DB) *gorm.DB {
		if query != "" {
			arr := strings.Fields(query)
			for i := 0; i < len(arr); i++ {
				if strings.HasPrefix(arr[i], "-") {
					q := "%" + arr[i][1:] + "%"
					session = session.Where("ident not like ? and host_ip not like ? and "+
						"note not like ? and tags not like ? and (host_tags not like ? or "+
						"host_tags is null) and os not like ?", q, q, q, q, q, q)
				} else {
					q := "%" + arr[i] + "%"
					session = session.Where("ident like ? or host_ip like ? or note like ? or "+
						"tags like ? or host_tags like ? or os like ?", q, q, q, q, q, q)
				}
			}
		}
		return session
	}
}

func BuildTargetWhereWithDowntime(downtime int64) BuildTargetWhereOption {
	return func(session *gorm.DB) *gorm.DB {
		if downtime > 0 {
			session = session.Where("target.update_at < ?", time.Now().Unix()-downtime)
		} else if downtime < 0 {
			session = session.Where("target.update_at > ?", time.Now().Unix()+downtime)
		}
		return session
	}
}

func buildTargetWhere(ctx *ctx.Context, options ...BuildTargetWhereOption) *gorm.DB {
	sub := DB(ctx).Model(&Target{}).Distinct("target.ident")
	for _, opt := range options {
		sub = opt(sub)
	}
	return DB(ctx).Model(&Target{}).Where("ident in (?)", sub)
}

func TargetTotal(ctx *ctx.Context, options ...BuildTargetWhereOption) (int64, error) {
	return Count(buildTargetWhere(ctx, options...))
}

func TargetGets(ctx *ctx.Context, limit, offset int, order string, desc bool, options ...BuildTargetWhereOption) ([]*Target, error) {
	var lst []*Target
	if desc {
		order += " desc"
	} else {
		order += " asc"
	}
	err := buildTargetWhere(ctx, options...).Order(order).Limit(limit).Offset(offset).Find(&lst).Error
	if err == nil {
		for i := 0; i < len(lst); i++ {
			lst[i].TagsJSON = strings.Fields(lst[i].Tags)
		}
	}
	return lst, err
}

// 根据 groupids, tags, hosts 查询 targets
func TargetGetsByFilter(ctx *ctx.Context, query []map[string]interface{}, limit, offset int) ([]*Target, error) {
	var lst []*Target
	session := TargetFilterQueryBuild(ctx, query, limit, offset)
	err := session.Order("ident").Find(&lst).Error
	cache := make(map[int64]*BusiGroup)
	for i := 0; i < len(lst); i++ {
		lst[i].TagsJSON = strings.Fields(lst[i].Tags)
		lst[i].FillGroup(ctx, cache)
	}

	return lst, err
}

func TargetCountByFilter(ctx *ctx.Context, query []map[string]interface{}) (int64, error) {
	session := TargetFilterQueryBuild(ctx, query, 0, 0)
	return Count(session)
}

func MissTargetGetsByFilter(ctx *ctx.Context, query []map[string]interface{}, ts int64) ([]*Target, error) {
	var lst []*Target
	session := TargetFilterQueryBuild(ctx, query, 0, 0)
	session = session.Where("update_at < ?", ts)

	err := session.Order("ident").Find(&lst).Error
	return lst, err
}

func MissTargetCountByFilter(ctx *ctx.Context, query []map[string]interface{}, ts int64) (int64, error) {
	session := TargetFilterQueryBuild(ctx, query, 0, 0)
	session = session.Where("update_at < ?", ts)
	return Count(session)
}

func TargetFilterQueryBuild(ctx *ctx.Context, query []map[string]interface{}, limit, offset int) *gorm.DB {
	sub := DB(ctx).Model(&Target{}).Distinct("target.ident").Joins("left join " +
		"target_busi_group on target.ident = target_busi_group.target_ident")
	for _, q := range query {
		tx := DB(ctx).Model(&Target{})
		for k, v := range q {
			if strings.Count(k, "?") > 1 {
				tx = tx.Or(k, v.([]interface{})...)
			} else {
				tx = tx.Or(k, v)
			}
		}
		sub = sub.Where(tx)
	}

	session := DB(ctx).Model(&Target{}).Where("ident in (?)", sub)

	if limit > 0 {
		session = session.Limit(limit).Offset(offset)
	}

	return session
}

func TargetGetsAll(ctx *ctx.Context) ([]*Target, error) {
	if !ctx.IsCenter {
		lst, err := poster.GetByUrls[[]*Target](ctx, "/v1/n9e/targets")
		return lst, err
	}

	var lst []*Target
	err := DB(ctx).Model(&Target{}).Find(&lst).Error
	if err != nil {
		return lst, err
	}

	tgs, err := TargetBusiGroupsGetAll(ctx)
	if err != nil {
		return lst, err
	}

	for i := 0; i < len(lst); i++ {
		lst[i].FillTagsMap()
		lst[i].GroupIds = tgs[lst[i].Ident]
	}

	return lst, err
}

func TargetUpdateNote(ctx *ctx.Context, idents []string, note string) error {
	return DB(ctx).Model(&Target{}).Where("ident in ?", idents).Updates(map[string]interface{}{
		"note":      note,
		"update_at": time.Now().Unix(),
	}).Error
}

func TargetUpdateBgid(ctx *ctx.Context, idents []string, bgid int64, clearTags bool) error {
	fields := map[string]interface{}{
		"group_id":  bgid,
		"update_at": time.Now().Unix(),
	}

	if clearTags {
		fields["tags"] = ""
	}

	return DB(ctx).Model(&Target{}).Where("ident in ?", idents).Updates(fields).Error
}

func TargetGet(ctx *ctx.Context, where string, args ...interface{}) (*Target, error) {
	var lst []*Target
	err := DB(ctx).Where(where, args...).Find(&lst).Error
	if err != nil {
		return nil, err
	}

	if len(lst) == 0 {
		return nil, nil
	}

	lst[0].TagsJSON = strings.Fields(lst[0].Tags)

	return lst[0], nil
}

func TargetGetById(ctx *ctx.Context, id int64) (*Target, error) {
	return TargetGet(ctx, "id = ?", id)
}

func TargetGetByIdent(ctx *ctx.Context, ident string) (*Target, error) {
	return TargetGet(ctx, "ident = ?", ident)
}

func TargetsGetByIdents(ctx *ctx.Context, idents []string) ([]*Target, error) {
	var targets []*Target
	err := DB(ctx).Where("ident IN ?", idents).Find(&targets).Error
	return targets, err
}

func TargetsGetIdentsByIdentsAndHostIps(ctx *ctx.Context, idents, hostIps []string) (map[string]string, []string, error) {
	inexistence := make(map[string]string)
	identSet := set.NewStringSet()

	// Query the ident corresponding to idents
	if len(idents) > 0 {
		var identsFromIdents []string
		err := DB(ctx).Model(&Target{}).Where("ident IN ?", idents).Pluck("ident", &identsFromIdents).Error
		if err != nil {
			return nil, nil, err
		}

		for _, ident := range identsFromIdents {
			identSet.Add(ident)
		}

		for _, ident := range idents {
			if !identSet.Exists(ident) {
				inexistence[ident] = "Ident not found"
			}
		}
	}

	// Query the hostIp corresponding to idents
	if len(hostIps) > 0 {
		var hostIpToIdentMap []struct {
			HostIp string
			Ident  string
		}
		err := DB(ctx).Model(&Target{}).Select("host_ip, ident").Where("host_ip IN ?", hostIps).Scan(&hostIpToIdentMap).Error
		if err != nil {
			return nil, nil, err
		}

		hostIpToIdent := set.NewStringSet()
		for _, entry := range hostIpToIdentMap {
			hostIpToIdent.Add(entry.HostIp)
			identSet.Add(entry.Ident)
		}

		for _, hostIp := range hostIps {
			if !hostIpToIdent.Exists(hostIp) {
				inexistence[hostIp] = "HostIp not found"
			}
		}
	}

	return inexistence, identSet.ToSlice(), nil
}

func TargetsGetIdsByIdentsAndHostIps(ctx *ctx.Context, idents, hostIps []string) (
	map[string]string, []int64, error) {
	inexistence := make(map[string]string)
	idSet := set.NewInt64Set()

	if len(idents) > 0 {
		var identToIdMap []struct {
			Ident string
			Id    int64
		}
		err := DB(ctx).Model(&Target{}).Select("id, ident").Where("ident IN ?", idents).Scan(&identToIdMap).Error
		if err != nil {
			return nil, nil, err
		}

		identSet := set.NewStringSet()
		for _, entry := range identToIdMap {
			idSet.Add(entry.Id)
			identSet.Add(entry.Ident)
		}

		for _, ident := range idents {
			if !identSet.Exists(ident) {
				inexistence[ident] = "Ident not found"
			}
		}
	}

	// Query the hostIp corresponding to idents
	if len(hostIps) > 0 {
		var hostIpMap []struct {
			HostIp string
			Ident  string
			Id     int64
		}
		err := DB(ctx).Model(&Target{}).Select("id, host_ip").Where("host_ip IN ?", hostIps).Scan(&hostIpMap).Error
		if err != nil {
			return nil, nil, err
		}

		hostIpSet := set.NewStringSet()
		for _, entry := range hostIpMap {
			hostIpSet.Add(entry.HostIp)
			idSet.Add(entry.Id)
		}

		for _, hostIp := range hostIps {
			if !hostIpSet.Exists(hostIp) {
				inexistence[hostIp] = "HostIp not found"
			}
		}
	}

	return inexistence, idSet.ToSlice(), nil
}

func TargetGetTags(ctx *ctx.Context, idents []string, ignoreHostTag bool, bgLabelKey string) (
	[]string, error) {
	session := DB(ctx).Model(new(Target))

	var arr []*Target
	if len(idents) > 0 {
		session = session.Where("ident in ?", idents)
	}

	err := session.Select("tags", "host_tags").Find(&arr).Error
	if err != nil {
		return nil, err
	}

	cnt := len(arr)
	if cnt == 0 {
		return []string{}, nil
	}

	set := make(map[string]struct{})
	for i := 0; i < cnt; i++ {
		tags := strings.Fields(arr[i].Tags)
		for j := 0; j < len(tags); j++ {
			set[tags[j]] = struct{}{}
		}

		if !ignoreHostTag {
			for _, ht := range arr[i].HostTags {
				set[ht] = struct{}{}
			}
		}
	}

	cnt = len(set)
	ret := make([]string, 0, cnt)
	for key := range set {
		ret = append(ret, key)
	}

	if bgLabelKey != "" {
		sort.Slice(ret, func(i, j int) bool {
			if strings.HasPrefix(ret[i], bgLabelKey) && strings.HasPrefix(ret[j], bgLabelKey) {
				return ret[i] < ret[j]
			}
			if strings.HasPrefix(ret[i], bgLabelKey) {
				return true
			}
			if strings.HasPrefix(ret[j], bgLabelKey) {
				return false
			}
			return ret[i] < ret[j]
		})
	} else {
		sort.Strings(ret)
	}

	return ret, err
}

func (t *Target) AddTags(ctx *ctx.Context, tags []string) error {
	for i := 0; i < len(tags); i++ {
		if !strings.Contains(t.Tags, tags[i]+" ") {
			t.Tags += tags[i] + " "
		}
	}

	arr := strings.Fields(t.Tags)
	sort.Strings(arr)

	return DB(ctx).Model(t).Updates(map[string]interface{}{
		"tags":      strings.Join(arr, " ") + " ",
		"update_at": time.Now().Unix(),
	}).Error
}

func (t *Target) DelTags(ctx *ctx.Context, tags []string) error {
	for _, tag := range tags {
		t.Tags = strings.ReplaceAll(t.Tags, tag+" ", "")
	}

	return DB(ctx).Model(t).Updates(map[string]interface{}{
		"tags":      t.Tags,
		"update_at": time.Now().Unix(),
	}).Error
}

func (t *Target) FillTagsMap() {
	t.TagsJSON = strings.Fields(t.Tags)
	t.TagsMap = make(map[string]string)
	m := make(map[string]string)
	allTags := append(t.TagsJSON, t.HostTags...)
	for _, item := range allTags {
		arr := strings.Split(item, "=")
		if len(arr) != 2 {
			continue
		}
		m[arr[0]] = arr[1]
	}

	t.TagsMap = m
}

func (t *Target) GetTagsMap() map[string]string {
	tagsJSON := strings.Fields(t.Tags)
	m := make(map[string]string)
	for _, item := range tagsJSON {
		if arr := strings.Split(item, "="); len(arr) == 2 {
			m[arr[0]] = arr[1]
		}
	}
	return m
}

func (t *Target) GetHostTagsMap() map[string]string {
	m := make(map[string]string)
	for _, item := range t.HostTags {
		arr := strings.Split(item, "=")
		if len(arr) != 2 {
			continue
		}
		m[arr[0]] = arr[1]
	}
	return m
}

func (t *Target) FillMeta(meta *HostMeta) {
	t.MemUtil = meta.MemUtil
	t.CpuUtil = meta.CpuUtil
	t.CpuNum = meta.CpuNum
	t.UnixTime = meta.UnixTime
	t.Offset = meta.Offset
	t.Arch = meta.Arch
	t.RemoteAddr = meta.RemoteAddr
}

func TargetIdents(ctx *ctx.Context, ids []int64) ([]string, error) {
	var ret []string

	if len(ids) == 0 {
		return ret, nil
	}

	err := DB(ctx).Model(&Target{}).Where("id in ?", ids).Pluck("ident", &ret).Error
	return ret, err
}

func TargetIds(ctx *ctx.Context, idents []string) ([]int64, error) {
	var ret []int64

	if len(idents) == 0 {
		return ret, nil
	}

	err := DB(ctx).Model(&Target{}).Where("ident in ?", idents).Pluck("id", &ret).Error
	return ret, err
}

func IdentsFilter(ctx *ctx.Context, idents []string, where string, args ...interface{}) ([]string, error) {
	var arr []string
	if len(idents) == 0 {
		return arr, nil
	}

	err := DB(ctx).Model(&Target{}).Where("ident in ?", idents).Where(where, args...).Pluck("ident", &arr).Error
	return arr, err
}

func (m *Target) UpdateFieldsMap(ctx *ctx.Context, fields map[string]interface{}) error {
	return DB(ctx).Model(m).Updates(fields).Error
}

// 1. 是否可以进行 busi_group 迁移
func CanMigrateBg(ctx *ctx.Context) bool {
	// 1.1 检查 target 表是否为空
	var cnt int64
	if err := DB(ctx).Model(&Target{}).Count(&cnt).Error; err != nil {
		log.Println("failed to get target table count, err:", err)
		return false
	}
	if cnt == 0 {
		log.Println("target table is empty, skip migration.")
		return false
	}

	// 1.2 判断是否已经完成迁移
	var maxGroupId int64
	if err := DB(ctx).Model(&Target{}).Select("MAX(group_id)").Scan(&maxGroupId).Error; err != nil {
		log.Println("failed to get max group_id from target table, err:", err)
		return false
	}

	if maxGroupId == 0 {
		return false
	}

	return true
}

func MigrateBg(ctx *ctx.Context, bgLabelKey string) {
	err := DoMigrateBg(ctx, bgLabelKey)
	if err != nil {
		log.Println("failed to migrate bgid, err:", err)
		return
	}

	log.Println("migration bgid has been completed")
}

func DoMigrateBg(ctx *ctx.Context, bgLabelKey string) error {
	// 2. 获取全量 target
	targets, err := TargetGetsAll(ctx)
	if err != nil {
		return err
	}

	// 3. 获取全量 busi_group
	bgs, err := BusiGroupGetAll(ctx)
	if err != nil {
		return err
	}

	bgById := make(map[int64]*BusiGroup, len(bgs))
	for _, bg := range bgs {
		bgById[bg.Id] = bg
	}

	// 4. 如果某 busi_group 有 label，将其存至对应的 target tags 中
	for _, t := range targets {
		if t.GroupId == 0 {
			continue
		}
		err := DB(ctx).Transaction(func(tx *gorm.DB) error {
			// 4.1 将 group_id 迁移至关联表
			if err := TargetBindBgids(ctx, []string{t.Ident}, []int64{t.GroupId}, nil); err != nil {
				return err
			}
			if err := TargetUpdateBgid(ctx, []string{t.Ident}, 0, false); err != nil {
				return err
			}

			// 4.2 判断该机器是否需要新增 tag
			if bg, ok := bgById[t.GroupId]; !ok || bg.LabelEnable == 0 ||
				strings.Contains(t.Tags, bgLabelKey+"=") {
				return nil
			} else {
				return t.AddTags(ctx, []string{bgLabelKey + "=" + bg.LabelValue})
			}
		})
		if err != nil {
			log.Printf("failed to migrate %v bg, err: %v\n", t.Ident, err)
			continue
		}
	}
	return nil
}
