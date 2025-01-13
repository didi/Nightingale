package migrate

import (
	"fmt"

	"github.com/ccfos/nightingale/v6/models"
	"github.com/ccfos/nightingale/v6/pkg/ormx"

	imodels "github.com/flashcatcloud/ibex/src/models"
	"github.com/toolkits/pkg/logger"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
)

func Migrate(db *gorm.DB) {
	MigrateTables(db)
	MigrateEsIndexPatternTable(db)
}

func MigrateIbexTables(db *gorm.DB) {
	var tableOptions string
	switch db.Dialector.(type) {
	case *mysql.Dialector:
		tableOptions = "ENGINE=InnoDB DEFAULT CHARSET=utf8mb4"
	}

	if tableOptions != "" {
		db = db.Set("gorm:table_options", tableOptions)
	}

	dts := []interface{}{&imodels.TaskMeta{}, &imodels.TaskScheduler{}, &TaskHostDoing{}, &imodels.TaskAction{}}
	for _, dt := range dts {
		err := db.AutoMigrate(dt)
		if err != nil {
			logger.Errorf("failed to migrate table:%v %v", dt, err)
		}
	}

	for i := 0; i < 100; i++ {
		tableName := fmt.Sprintf("task_host_%d", i)
		exists := db.Migrator().HasTable(tableName)
		if exists {
			continue
		} else {
			err := db.Table(tableName).AutoMigrate(&imodels.TaskHost{})
			if err != nil {
				logger.Errorf("failed to migrate table:%s %v", tableName, err)
			}
		}
	}
}

func isPostgres(db *gorm.DB) bool {
	dialect := db.Dialector.Name()
	return dialect == "postgres"
}
func MigrateTables(db *gorm.DB) error {
	var tableOptions string
	switch db.Dialector.(type) {
	case *mysql.Dialector:
		tableOptions = "ENGINE=InnoDB DEFAULT CHARSET=utf8mb4"
	}
	if tableOptions != "" {
		db = db.Set("gorm:table_options", tableOptions)
	}
	dts := []interface{}{&RecordingRule{}, &AlertRule{}, &AlertSubscribe{}, &AlertMute{},
		&TaskRecord{}, &ChartShare{}, &Target{}, &Configs{}, &Datasource{}, &NotifyTpl{},
		&Board{}, &BoardBusigroup{}, &Users{}, &SsoConfig{}, &models.BuiltinMetric{},
		&models.MetricFilter{}, &models.NotificaitonRecord{}, models.Token{},
		&models.TargetBusiGroup{}, &EsIndexPatternMigrate{}, &DashAnnotation{}}

	if isPostgres(db) {
		dts = append(dts, &models.PostgresBuiltinComponent{})
	} else {
		dts = append(dts, &models.BuiltinComponent{})
	}

	if !db.Migrator().HasColumn(&imodels.TaskSchedulerHealth{}, "scheduler") {
		dts = append(dts, &imodels.TaskSchedulerHealth{})
	}

	if !columnHasIndex(db, &AlertHisEvent{}, "original_tags") ||
		!columnHasIndex(db, &AlertCurEvent{}, "original_tags") {
		asyncDts := []interface{}{&AlertHisEvent{}, &AlertCurEvent{}}

		go func() {
			defer func() {
				if r := recover(); r != nil {
					logger.Errorf("panic to migrate table: %v", r)
				}
			}()

			for _, dt := range asyncDts {
				if err := db.AutoMigrate(dt); err != nil {
					logger.Errorf("failed to migrate table %+v err:%v", dt, err)
				}
			}
		}()
	}

	if !db.Migrator().HasTable(&models.BuiltinPayload{}) {
		dts = append(dts, &models.BuiltinPayload{})
	} else {
		dts = append(dts, &BuiltinPayloads{})
	}

	for _, dt := range dts {
		err := db.AutoMigrate(dt)
		if err != nil {
			logger.Errorf("failed to migrate table:%v %v", dt, err)
		}
	}

	if db.Migrator().HasColumn(&AlertingEngines{}, "cluster") {
		err := db.Migrator().RenameColumn(&AlertingEngines{}, "cluster", "engine_cluster")
		if err != nil {
			logger.Errorf("failed to renameColumn table: %v", err)
		}
	}

	if db.Migrator().HasColumn(&ChartShare{}, "dashboard_id") {
		err := db.Migrator().DropColumn(&ChartShare{}, "dashboard_id")
		if err != nil {
			logger.Errorf("failed to DropColumn table: %v", err)
		}
	}
	DropUniqueFiledLimit(db, &Configs{}, "ckey", "configs_ckey_key")
	InsertPermPoints(db)
	return nil
}

func DropUniqueFiledLimit(db *gorm.DB, dst interface{}, uniqueFiled string, pgUniqueFiled string) { // UNIQUE KEY (`ckey`)
	if db.Migrator().HasIndex(dst, uniqueFiled) {
		err := db.Migrator().DropIndex(dst, uniqueFiled) //mysql  DROP INDEX
		if err != nil {
			logger.Errorf("failed to DropIndex(%s) error: %v", uniqueFiled, err)
		}
	}
	if db.Migrator().HasConstraint(dst, pgUniqueFiled) {
		err := db.Migrator().DropConstraint(dst, pgUniqueFiled) //pg  DROP CONSTRAINT
		if err != nil {
			logger.Errorf("failed to DropConstraint(%s) error: %v", pgUniqueFiled, err)
		}
	}
}

func columnHasIndex(db *gorm.DB, dst interface{}, indexColumn string) bool {
	indexes, err := db.Migrator().GetIndexes(dst)
	if err != nil {
		logger.Errorf("failed to table getIndexes: %v", err)
		return false
	}
	for i := range indexes {
		for j := range indexes[i].Columns() {
			if indexes[i].Columns()[j] == indexColumn {
				return true
			}
		}
	}
	return false
}

func InsertPermPoints(db *gorm.DB) {
	var ops []models.RoleOperation
	ops = append(ops, models.RoleOperation{
		RoleName:  "Standard",
		Operation: "/alert-mutes/put",
	})

	ops = append(ops, models.RoleOperation{
		RoleName:  "Standard",
		Operation: "/log/index-patterns",
	})

	ops = append(ops, models.RoleOperation{
		RoleName:  "Standard",
		Operation: "/help/variable-configs",
	})

	ops = append(ops, models.RoleOperation{
		RoleName:  "Admin",
		Operation: "/permissions",
	})

	ops = append(ops, models.RoleOperation{
		RoleName:  "Standard",
		Operation: "/ibex-settings",
	})

	for _, op := range ops {
		var count int64

		err := db.Raw("SELECT COUNT(*) FROM role_operation WHERE operation = ? AND role_name = ?",
			op.Operation, op.RoleName).Scan(&count).Error

		if err != nil {
			logger.Errorf("check role operation exists failed, %v", err)
			continue
		}

		if count > 0 {
			continue
		}

		err = db.Create(&op).Error
		if err != nil {
			logger.Errorf("insert role operation failed, %v", err)
		}
	}
}

type AlertRule struct {
	ExtraConfig       string                   `gorm:"type:text;column:extra_config"`
	CronPattern       string                   `gorm:"type:varchar(64);column:cron_pattern"`
	DatasourceQueries []models.DatasourceQuery `gorm:"datasource_queries;type:text;serializer:json"` // datasource queries
}

type AlertSubscribe struct {
	ExtraConfig string       `gorm:"type:text;column:extra_config"` // extra config
	Severities  string       `gorm:"column:severities;type:varchar(32);not null;default:''"`
	BusiGroups  ormx.JSONArr `gorm:"column:busi_groups;type:varchar(4096)"`
	Note        string       `gorm:"column:note;type:varchar(1024);default:'';comment:note"`
	RuleIds     []int64      `gorm:"column:rule_ids;type:varchar(1024)"`
}

type AlertMute struct {
	Severities string `gorm:"column:severities;type:varchar(32);not null;default:''"`
	Tags       string `gorm:"column:tags;type:varchar(4096);default:'[]';comment:json,map,tagkey->regexp|value"`
}

type RecordingRule struct {
	QueryConfigs      string                   `gorm:"type:text;not null;column:query_configs"` // query_configs
	DatasourceIds     string                   `gorm:"column:datasource_ids;type:varchar(255);default:'';comment:datasource ids"`
	CronPattern       string                   `gorm:"column:cron_pattern;type:varchar(255);default:'';comment:cron pattern"`
	DatasourceQueries []models.DatasourceQuery `json:"datasource_queries" gorm:"datasource_queries;type:text;serializer:json"` // datasource queries
}

type AlertingEngines struct {
	EngineCluster string `gorm:"column:engine_cluster;type:varchar(128);default:'';comment:n9e engine cluster"`
}

type ChartShare struct {
	DatasourceId int64 `gorm:"column:datasource_id;bigint(20);not null;default:0;comment:datasource id"`
}
type TaskRecord struct {
	EventId int64 `gorm:"column:event_id;bigint(20);not null;default:0;comment:event id;index:idx_event_id"`
}
type AlertHisEvent struct {
	LastEvalTime int64  `gorm:"column:last_eval_time;bigint(20);not null;default:0;comment:for time filter;index:idx_last_eval_time"`
	OriginalTags string `gorm:"column:original_tags;type:text;comment:labels key=val,,k2=v2"`
}

type AlertCurEvent struct {
	OriginalTags string `gorm:"column:original_tags;type:text;comment:labels key=val,,k2=v2"`
}

type Target struct {
	HostIp       string   `gorm:"column:host_ip;type:varchar(15);default:'';comment:IPv4 string;index:idx_host_ip"`
	AgentVersion string   `gorm:"column:agent_version;type:varchar(255);default:'';comment:agent version;index:idx_agent_version"`
	EngineName   string   `gorm:"column:engine_name;type:varchar(255);default:'';comment:engine name;index:idx_engine_name"`
	OS           string   `gorm:"column:os;type:varchar(31);default:'';comment:os type;index:idx_os"`
	HostTags     []string `gorm:"column:host_tags;type:text;comment:global labels set in conf file;serializer:json"`
}

type Datasource struct {
	IsDefault bool `gorm:"column:is_default;type:boolean;comment:is default datasource"`
}

type Configs struct {
	Note string `gorm:"column:note;type:varchar(1024);default:'';comment:note"`
	Cval string `gorm:"column:cval;type:text;comment:config value"`
	//mysql tinyint//postgresql smallint
	External  int    `gorm:"column:external;type:int;default:0;comment:0\\:built-in 1\\:external"`
	Encrypted int    `gorm:"column:encrypted;type:int;default:0;comment:0\\:plaintext 1\\:ciphertext"`
	CreateAt  int64  `gorm:"column:create_at;type:int;default:0;comment:create_at"`
	CreateBy  string `gorm:"column:create_by;type:varchar(64);default:'';comment:cerate_by"`
	UpdateAt  int64  `gorm:"column:update_at;type:int;default:0;comment:update_at"`
	UpdateBy  string `gorm:"column:update_by;type:varchar(64);default:'';comment:update_by"`
}

type NotifyTpl struct {
	CreateAt int64  `gorm:"column:create_at;type:int;default:0;comment:create_at"`
	CreateBy string `gorm:"column:create_by;type:varchar(64);default:'';comment:cerate_by"`
	UpdateAt int64  `gorm:"column:update_at;type:int;default:0;comment:update_at"`
	UpdateBy string `gorm:"column:update_by;type:varchar(64);default:'';comment:update_by"`
}

type Board struct {
	PublicCate int `gorm:"column:public_cate;int;not null;default:0;comment:0 anonymous 1 login 2 busi"`
}

type BoardBusigroup struct {
	BusiGroupId int64 `gorm:"column:busi_group_id;bigint(20);not null;default:0;comment:busi group id"`
	BoardId     int64 `gorm:"column:board_id;bigint(20);not null;default:0;comment:board id"`
}

type Users struct {
	Belong         string `gorm:"column:belong;varchar(16);default:'';comment:belong"`
	LastActiveTime int64  `gorm:"column:last_active_time;type:int;default:0;comment:last_active_time"`
}

type SsoConfig struct {
	UpdateAt int64 `gorm:"column:update_at;type:int;default:0;comment:update_at"`
}

type BuiltinPayloads struct {
	UUID        int64 `json:"uuid" gorm:"type:bigint;not null;index:idx_uuid;comment:'uuid of payload'"`
	ComponentID int64 `json:"component_id" gorm:"type:bigint;index:idx_component,sort:asc;not null;default:0;comment:'component_id of payload'"`
}

type TaskHostDoing struct {
	Id             int64  `gorm:"column:id;index;primaryKey:false"`
	Host           string `gorm:"column:host;size:128;not null;index"`
	Clock          int64  `gorm:"column:clock;not null;default:0"`
	Action         string `gorm:"column:action;size:16;not null"`
	AlertTriggered bool   `gorm:"-"`
}

func (TaskHostDoing) TableName() string {
	return "task_host_doing"
}

type EsIndexPatternMigrate struct {
	CrossClusterEnabled int `gorm:"column:cross_cluster_enabled;type:int;default:0"`
}

func (EsIndexPatternMigrate) TableName() string {
	return "es_index_pattern"
}

type DashAnnotation struct {
	Id          int64  `gorm:"column:id;primaryKey;autoIncrement"`
	DashboardId int64  `gorm:"column:dashboard_id;not null"`
	PanelId     string `gorm:"column:panel_id;type:varchar(191);not null"`
	Tags        string `gorm:"column:tags;type:text"`
	Description string `gorm:"column:description;type:text"`
	Config      string `gorm:"column:config;type:text"`
	TimeStart   int64  `gorm:"column:time_start;not null;default:0"`
	TimeEnd     int64  `gorm:"column:time_end;not null;default:0"`
	CreateAt    int64  `gorm:"column:create_at;not null;default:0"`
	CreateBy    string `gorm:"column:create_by;type:varchar(64);not null;default:''"`
	UpdateAt    int64  `gorm:"column:update_at;not null;default:0"`
	UpdateBy    string `gorm:"column:update_by;type:varchar(64);not null;default:''"`
}

func (DashAnnotation) TableName() string {
	return "dash_annotation"
}
