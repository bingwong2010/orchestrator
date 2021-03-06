/*
   Copyright 2014 Outbrain Inc.

   Licensed under the Apache License, Version 2.0 (the "License");
   you may not use this file except in compliance with the License.
   You may obtain a copy of the License at

       http://www.apache.org/licenses/LICENSE-2.0

   Unless required by applicable law or agreed to in writing, software
   distributed under the License is distributed on an "AS IS" BASIS,
   WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
   See the License for the specific language governing permissions and
   limitations under the License.
*/

package inst

import (
	"database/sql"
	"errors"
	"fmt"
	"github.com/outbrain/golib/log"
	"github.com/outbrain/golib/sqlutils"
	"github.com/outbrain/orchestrator/config"
	"github.com/outbrain/orchestrator/db"
	"regexp"
	"strings"
	"time"
)

const SQLThreadPollDuration = 200 * time.Millisecond

const backendDBConcurrency = 20

var instanceReadChan = make(chan bool, backendDBConcurrency)
var instanceWriteChan = make(chan bool, backendDBConcurrency)

var detachPattern *regexp.Regexp

// Max concurrency for bulk topology operations
const topologyConcurrency = 100

var topologyConcurrencyChan = make(chan bool, topologyConcurrency)

func init() {
	detachPattern, _ = regexp.Compile(`//([^/:]+):([\d]+)`)
}

// ExecuteOnTopology will execute given function while maintaining concurrency limit
// on topology servers. It is safe in the sense that we will not leak tokens.
func ExecuteOnTopology(f func()) {
	topologyConcurrencyChan <- true
	defer func() { <-topologyConcurrencyChan }()
	f()
}

// ExecDBWriteFunc chooses how to execute a write onto the database: whether synchronuously or not
func ExecDBWriteFunc(f func() error) error {
	instanceWriteChan <- true
	defer func() { <-instanceWriteChan }()
	res := f()
	return res
}

// ExecInstance executes a given query on the given MySQL topology instance
func ExecInstance(instanceKey *InstanceKey, query string, args ...interface{}) (sql.Result, error) {
	db, err := db.OpenTopology(instanceKey.Hostname, instanceKey.Port)
	if err != nil {
		return nil, err
	}
	res, err := sqlutils.Exec(db, query, args...)
	return res, err
}

// ScanInstanceRow executes a read-a-single-row query on a given MySQL topology instance
func ScanInstanceRow(instanceKey *InstanceKey, query string, dest ...interface{}) error {
	db, err := db.OpenTopology(instanceKey.Hostname, instanceKey.Port)
	if err != nil {
		return err
	}
	err = db.QueryRow(query).Scan(dest...)
	return err
}

// ReadTopologyInstance connects to a topology MySQL instance and reads its configuration and
// replication status. It writes read info into orchestrator's backend.
func ReadTopologyInstance(instanceKey *InstanceKey) (*Instance, error) {
	defer func() {
		if err := recover(); err != nil {
			log.Errorf("Unexpected error: %+v", err)
		}
	}()

	instance := NewInstance()
	instanceFound := false
	foundBySlaveHosts := false
	longRunningProcesses := []Process{}
	resolvedHostname := ""
	var resolveErr error

	_ = UpdateInstanceLastAttemptedCheck(instanceKey)

	db, err := db.OpenTopology(instanceKey.Hostname, instanceKey.Port)

	if err != nil {
		goto Cleanup
	}

	instance.Key = *instanceKey
	err = db.QueryRow("select @@hostname, @@global.server_id, @@global.version, @@global.read_only, @@global.binlog_format, @@global.log_bin, @@global.log_slave_updates").Scan(
		&resolvedHostname, &instance.ServerID, &instance.Version, &instance.ReadOnly, &instance.Binlog_format, &instance.LogBinEnabled, &instance.LogSlaveUpdatesEnabled)
	if err != nil {
		goto Cleanup
	}
	if resolvedHostname != instance.Key.Hostname {
		UpdateResolvedHostname(instance.Key.Hostname, resolvedHostname)
		instance.Key.Hostname = resolvedHostname
	}
	err = sqlutils.QueryRowsMap(db, "show slave status", func(m sqlutils.RowMap) error {
		instance.Slave_IO_Running = (m.GetString("Slave_IO_Running") == "Yes")
		instance.Slave_SQL_Running = (m.GetString("Slave_SQL_Running") == "Yes")
		instance.ReadBinlogCoordinates.LogFile = m.GetString("Master_Log_File")
		instance.ReadBinlogCoordinates.LogPos = m.GetInt64("Read_Master_Log_Pos")
		instance.ExecBinlogCoordinates.LogFile = m.GetString("Relay_Master_Log_File")
		instance.ExecBinlogCoordinates.LogPos = m.GetInt64("Exec_Master_Log_Pos")
		instance.RelaylogCoordinates.LogFile = m.GetString("Relay_Log_File")
		instance.RelaylogCoordinates.LogPos = m.GetInt64("Relay_Log_Pos")
		instance.RelaylogCoordinates.Type = RelayLog
		instance.LastSQLError = m.GetString("Last_SQL_Error")
		instance.LastIOError = m.GetString("Last_IO_Error")
		instance.UsingOracleGTID = (m.GetIntD("Auto_Position", 0) == 1)
		instance.UsingMariaDBGTID = (m.GetStringD("Using_Gtid", "No") == "Yes")

		masterKey, err := NewInstanceKeyFromStrings(m.GetString("Master_Host"), m.GetString("Master_Port"))
		if err != nil {
			log.Errore(err)
		}
		masterKey.Hostname, resolveErr = ResolveHostname(masterKey.Hostname)
		if resolveErr != nil {
			log.Errore(resolveErr)
		}
		instance.MasterKey = *masterKey
		instance.SecondsBehindMaster = m.GetNullInt64("Seconds_Behind_Master")
		if config.Config.SlaveLagQuery == "" {
			instance.SlaveLagSeconds = instance.SecondsBehindMaster
		}
		// Not breaking the flow even on error
		return nil
	})
	if err != nil {
		goto Cleanup
	}

	if instance.LogBinEnabled {
		err = sqlutils.QueryRowsMap(db, "show master status", func(m sqlutils.RowMap) error {
			var err error
			instance.SelfBinlogCoordinates.LogFile = m.GetString("File")
			instance.SelfBinlogCoordinates.LogPos = m.GetInt64("Position")
			return err
		})
		if err != nil {
			goto Cleanup
		}
	}

	// Get slaves, either by SHOW SLAVE HOSTS or via PROCESSLIST
	if config.Config.DiscoverByShowSlaveHosts {
		err := sqlutils.QueryRowsMap(db, `show slave hosts`,
			func(m sqlutils.RowMap) error {
				slaveKey, err := NewInstanceKeyFromStrings(m.GetString("Host"), m.GetString("Port"))
				slaveKey.Hostname, resolveErr = ResolveHostname(slaveKey.Hostname)
				if err == nil {
					instance.AddSlaveKey(slaveKey)
					foundBySlaveHosts = true
				}
				return err
			})

		if err != nil {
			goto Cleanup
		}
	}
	if !foundBySlaveHosts {
		// Either not configured to read SHOW SLAVE HOSTS or nothing was there.
		// Discover by processlist
		err := sqlutils.QueryRowsMap(db, `
        	select 
        		substring_index(host, ':', 1) as slave_hostname 
        	from 
        		information_schema.processlist 
        	where 
        		command='Binlog Dump'`,
			func(m sqlutils.RowMap) error {
				cname, resolveErr := ResolveHostname(m.GetString("slave_hostname"))
				if resolveErr != nil {
					log.Errore(resolveErr)
				}
				slaveKey := InstanceKey{Hostname: cname, Port: instance.Key.Port}
				instance.AddSlaveKey(&slaveKey)
				return err
			})

		if err != nil {
			goto Cleanup
		}
	}
	{
		binlogs := []string{}
		if instance.LogBinEnabled {
			// Get binary (master) logs
			err = sqlutils.QueryRowsMap(db, "show binary logs", func(m sqlutils.RowMap) error {
				binlogs = append(binlogs, m.GetString("Log_name"))
				return nil
			})
			if err != nil {
				goto Cleanup
			}
		}
		instance.SetBinaryLogs(binlogs)
	}
	instanceFound = true
	// Anything after this point does not affect the fact the instance is found.
	{
		// Get long running processes
		err := sqlutils.QueryRowsMap(db, `
				  select 
				    id,
				    user,
				    host,
				    db,
				    command,
				    time,
				    state,
				    left(processlist.info, 1024) as info,
				    now() - interval time second as started_at
				  from 
				    information_schema.processlist 
				  where
				    time > 60
				    and command != 'Sleep'
				    and id != connection_id()
				    and user != 'system user' 
				    and command != 'Binlog dump'
				    and user != 'event_scheduler'
				  order by
				    time desc
        		`,
			func(m sqlutils.RowMap) error {
				process := Process{}
				process.Id = m.GetInt64("id")
				process.User = m.GetString("user")
				process.Host = m.GetString("host")
				process.Db = m.GetString("db")
				process.Command = m.GetString("command")
				process.Time = m.GetInt64("time")
				process.State = m.GetString("state")
				process.Info = m.GetString("info")
				process.StartedAt = m.GetString("started_at")

				longRunningProcesses = append(longRunningProcesses, process)
				return nil
			})

		if err != nil {
			goto Cleanup
		}
	}

	if config.Config.SlaveLagQuery != "" {
		err = db.QueryRow(config.Config.SlaveLagQuery).Scan(&instance.SlaveLagSeconds)
		if err != nil {
			goto Cleanup
		}
	}

	instance.ClusterName, instance.ReplicationDepth, err = ReadClusterNameByMaster(&instance.Key, &instance.MasterKey)
	if err != nil {
		goto Cleanup
	}

Cleanup:
	if instanceFound {
		_ = writeInstance(instance, instanceFound, err)
		WriteLongRunningProcesses(&instance.Key, longRunningProcesses)
	} else {
		_ = UpdateInstanceLastChecked(&instance.Key)
	}
	if err != nil {
		log.Errore(err)
	}
	return instance, err
}

// ReadClusterNameByMaster will return the cluster name for a given instance by looking at its master
// and getting it from there.
// It is a non-recursive function and so-called-recursion is performed upon periodic reading of
// instances.
func ReadClusterNameByMaster(instanceKey *InstanceKey, masterKey *InstanceKey) (string, uint, error) {
	db, err := db.OpenOrchestrator()
	if err != nil {
		return "", 0, log.Errore(err)
	}

	var clusterName string
	var replicationDepth uint
	err = db.QueryRow(`
       	select 
       		if (
       			cluster_name != '',
       			cluster_name,
	       		ifnull(concat(max(hostname), ':', max(port)), '')
	       	) as cluster_name,
	       	ifnull(max(replication_depth)+1, 0) as replication_depth
       	from database_instance 
		 	where hostname=? and port=?`,
		masterKey.Hostname, masterKey.Port).Scan(
		&clusterName, &replicationDepth,
	)

	if err != nil {
		return "", 0, log.Errore(err)
	}
	if clusterName == "" {
		clusterName = fmt.Sprintf("%s:%d", instanceKey.Hostname, instanceKey.Port)
	}
	return clusterName, replicationDepth, err
}

// readInstanceRow reads a single instance row from the orchestrator backend database.
func readInstanceRow(m sqlutils.RowMap) *Instance {
	instance := NewInstance()

	instance.Key.Hostname = m.GetString("hostname")
	instance.Key.Port = m.GetInt("port")
	instance.ServerID = m.GetUint("server_id")
	instance.Version = m.GetString("version")
	instance.ReadOnly = m.GetBool("read_only")
	instance.Binlog_format = m.GetString("binlog_format")
	instance.LogBinEnabled = m.GetBool("log_bin")
	instance.LogSlaveUpdatesEnabled = m.GetBool("log_slave_updates")
	instance.MasterKey.Hostname = m.GetString("master_host")
	instance.MasterKey.Port = m.GetInt("master_port")
	instance.Slave_SQL_Running = m.GetBool("slave_sql_running")
	instance.Slave_IO_Running = m.GetBool("slave_io_running")
	instance.UsingOracleGTID = m.GetBool("oracle_gtid")
	instance.UsingMariaDBGTID = m.GetBool("mariadb_gtid")
	instance.UsingPseudoGTID = m.GetBool("pseudo_gtid")
	instance.SelfBinlogCoordinates.LogFile = m.GetString("binary_log_file")
	instance.SelfBinlogCoordinates.LogPos = m.GetInt64("binary_log_pos")
	instance.ReadBinlogCoordinates.LogFile = m.GetString("master_log_file")
	instance.ReadBinlogCoordinates.LogPos = m.GetInt64("read_master_log_pos")
	instance.ExecBinlogCoordinates.LogFile = m.GetString("relay_master_log_file")
	instance.ExecBinlogCoordinates.LogPos = m.GetInt64("exec_master_log_pos")
	instance.RelaylogCoordinates.LogFile = m.GetString("relay_log_file")
	instance.RelaylogCoordinates.LogPos = m.GetInt64("relay_log_pos")
	instance.RelaylogCoordinates.Type = RelayLog
	instance.LastSQLError = m.GetString("last_sql_error")
	instance.LastIOError = m.GetString("last_io_error")
	instance.SecondsBehindMaster = m.GetNullInt64("seconds_behind_master")
	instance.SlaveLagSeconds = m.GetNullInt64("slave_lag_seconds")
	slaveHostsJson := m.GetString("slave_hosts")
	instance.ClusterName = m.GetString("cluster_name")
	instance.ReplicationDepth = m.GetUint("replication_depth")
	instance.IsUpToDate = (m.GetUint("seconds_since_last_checked") <= config.Config.InstancePollSeconds)
	instance.IsRecentlyChecked = (m.GetUint("seconds_since_last_checked") <= config.Config.InstancePollSeconds*5)
	instance.IsLastCheckValid = m.GetBool("is_last_check_valid")
	instance.SecondsSinceLastSeen = m.GetNullInt64("seconds_since_last_seen")

	instance.ReadSlaveHostsFromJson(slaveHostsJson)
	return instance
}

// readInstancesByCondition is a generic function to read instances from the backend database
func readInstancesByCondition(condition string) ([](*Instance), error) {
	readFunc := func() ([](*Instance), error) {
		instances := [](*Instance){}

		db, err := db.OpenOrchestrator()
		if err != nil {
			return instances, log.Errore(err)
		}

		query := fmt.Sprintf(`
		select 
			*,
			timestampdiff(second, last_checked, now()) as seconds_since_last_checked,
			(last_checked <= last_seen) is true as is_last_check_valid,
			timestampdiff(second, last_seen, now()) as seconds_since_last_seen
		from 
			database_instance 
		where
			%s
		order by
			hostname, port`, condition)

		err = sqlutils.QueryRowsMap(db, query, func(m sqlutils.RowMap) error {
			instance := readInstanceRow(m)
			instances = append(instances, instance)
			return nil
		})
		if err != nil {
			return instances, log.Errore(err)
		}
		err = PopulateInstancesAgents(instances)
		if err != nil {
			return instances, log.Errore(err)
		}
		return instances, err
	}
	instanceReadChan <- true
	instances, err := readFunc()
	<-instanceReadChan
	return instances, err
}

// ReadInstance reads an instance from the orchestrator backend database
func ReadInstance(instanceKey *InstanceKey) (*Instance, bool, error) {
	condition := fmt.Sprintf(`
			hostname = '%s'
			and port = %d
		`, instanceKey.Hostname, instanceKey.Port)
	instances, err := readInstancesByCondition(condition)
	// We know there will be at most one (hostname & port are PK)
	// And we expect to find one
	if len(instances) == 0 {
		return nil, false, err
	}
	if err != nil {
		return instances[0], false, err
	}
	return instances[0], true, nil
}

// ReadClusterInstances reads all instances of a given cluster
func ReadClusterInstances(clusterName string) ([](*Instance), error) {
	if strings.Index(clusterName, "'") >= 0 {
		return [](*Instance){}, log.Errorf("Invalid cluster name: %s", clusterName)
	}
	condition := fmt.Sprintf(`cluster_name = '%s'`, clusterName)
	return readInstancesByCondition(condition)
}

// ReadSlaveInstances reads slaves of a given master
func ReadSlaveInstances(masterKey *InstanceKey) ([](*Instance), error) {
	condition := fmt.Sprintf(`
			master_host = '%s'
			and master_port = %d
		`, masterKey.Hostname, masterKey.Port)
	return readInstancesByCondition(condition)
}

// ReadUnseenInstances reads all instances which were not recently seen
func ReadUnseenInstances() ([](*Instance), error) {
	condition := fmt.Sprintf(`
			last_seen < last_checked
		`)
	return readInstancesByCondition(condition)
}

// ReadProblemInstances reads all instances with problems
func ReadProblemInstances() ([](*Instance), error) {
	condition := fmt.Sprintf(`
			(last_seen < last_checked)
			or (not ifnull(timestampdiff(second, last_checked, now()) <= %d, false))
			or (not slave_sql_running)
			or (not slave_io_running)
			or (seconds_behind_master > 10)
		`, config.Config.InstancePollSeconds)
	return readInstancesByCondition(condition)
}

// SearchInstances reads all instances qualifying for some searchString
func SearchInstances(searchString string) ([](*Instance), error) {
	condition := fmt.Sprintf(`
			hostname like '%%%s%%'
			or cluster_name like '%%%s%%'
			or server_id = '%s'
			or version like '%%%s%%'
			or port = '%s'
			or concat(hostname, ':', port) like '%%%s%%'
		`, searchString, searchString, searchString, searchString, searchString, searchString)
	return readInstancesByCondition(condition)
}

// FindInstances reads all instances whose name matches given pattern
func FindInstances(regexpPattern string) ([](*Instance), error) {
	condition := fmt.Sprintf(`
			hostname rlike '%s'
		`, regexpPattern)
	return readInstancesByCondition(condition)
}

// updateClusterNameForUnseenInstances
func updateInstanceClusterName(instance *Instance) error {
	writeFunc := func() error {
		db, err := db.OpenOrchestrator()
		if err != nil {
			return log.Errore(err)
		}
		_, err = sqlutils.Exec(db, `
			update 
				database_instance 
			set 
				cluster_name=? 
			where 
				hostname=? and port=?
        	`, instance.ClusterName, instance.Key.Hostname, instance.Key.Port,
		)
		if err != nil {
			return log.Errore(err)
		}
		AuditOperation("update-cluster-name", &instance.Key, fmt.Sprintf("set to %s", instance.ClusterName))
		return nil
	}
	return ExecDBWriteFunc(writeFunc)
}

// ReviewUnseenInstances reviews instances that have not been seen (suposedly dead) and updates some of their data
func ReviewUnseenInstances() error {
	instances, err := ReadUnseenInstances()
	if err != nil {
		return log.Errore(err)
	}
	operations := 0
	for _, instance := range instances {
		instance := instance

		masterHostname, err := ResolveHostname(instance.MasterKey.Hostname)
		if err != nil {
			log.Errore(err)
			continue
		}
		instance.MasterKey.Hostname = masterHostname
		clusterName, replicationDepth, err := ReadClusterNameByMaster(&instance.Key, &instance.MasterKey)

		if err != nil {
			log.Errore(err)
		} else if clusterName != instance.ClusterName {
			instance.ClusterName = clusterName
			instance.ReplicationDepth = replicationDepth
			updateInstanceClusterName(instance)
			operations++
		}
	}

	AuditOperation("review-unseen-instances", nil, fmt.Sprintf("Operations: %d", operations))
	return err
}

// readUnseenMasterKeys will read list of masters that have never been seen, and yet whose slaves
// seem to be replicating.
func readUnseenMasterKeys() ([]InstanceKey, error) {
	res := []InstanceKey{}
	db, err := db.OpenOrchestrator()
	if err != nil {
		return res, log.Errore(err)
	}
	err = sqlutils.QueryRowsMap(db, `
			SELECT DISTINCT
			    slave_instance.master_host, slave_instance.master_port
			FROM
			    database_instance slave_instance
			        LEFT JOIN
			    hostname_resolve ON (slave_instance.master_host = hostname_resolve.hostname)
			        LEFT JOIN
			    database_instance master_instance ON (
			    	COALESCE(hostname_resolve.resolved_hostname, slave_instance.master_host) = master_instance.hostname
			    	and slave_instance.master_port = master_instance.port)
			WHERE
			    master_instance.last_checked IS NULL
			    and slave_instance.master_host != ''
			    and slave_instance.master_host != '_'
			    and slave_instance.master_port > 0
			    and slave_instance.slave_io_running = 1
			`, func(m sqlutils.RowMap) error {
		instanceKey, _ := NewInstanceKeyFromStrings(m.GetString("master_host"), m.GetString("master_port"))
		// we ignore the error. It can be expected that we are unable to resolve the hostname.
		// Maybe that's how we got here in the first place!
		res = append(res, *instanceKey)

		return nil
	})
	if err != nil {
		return res, log.Errore(err)
	}

	return res, nil

}

// InjectUnseenMasters will review masters of instances that are known to be replication, yet which are not listed
// in database_instance. Since their slaves are listed as replicating, we can assume that such masters actually do
// exist: we shall therefore inject them with minimal details into the database_instance table.
func InjectUnseenMasters() error {

	unseenMasterKeys, err := readUnseenMasterKeys()
	if err != nil {
		return err
	}

	operations := 0
	for _, masterKey := range unseenMasterKeys {
		masterKey := masterKey
		clusterName := fmt.Sprintf("%s:%d", masterKey.Hostname, masterKey.Port)
		// minimal details:
		instance := Instance{Key: masterKey, Version: "Unknown", ClusterName: clusterName}
		if err := writeInstance(&instance, false, nil); err == nil {
			operations++
		}
	}

	AuditOperation("inject-unseen-masters", nil, fmt.Sprintf("Operations: %d", operations))
	return err
}

// ReadCountMySQLSnapshots is a utility method to return registered number of snapshots for a given list of hosts
func ReadCountMySQLSnapshots(hostnames []string) (map[string]int, error) {
	res := make(map[string]int)
	if !config.Config.ServeAgentsHttp {
		return res, nil
	}
	query := fmt.Sprintf(`
		select 
			hostname,
			count_mysql_snapshots
		from 
			host_agent
		where
			hostname in (%s)
		order by
			hostname
		`, sqlutils.InClauseStringValues(hostnames))
	db, err := db.OpenOrchestrator()
	if err != nil {
		goto Cleanup
	}

	err = sqlutils.QueryRowsMap(db, query, func(m sqlutils.RowMap) error {
		res[m.GetString("hostname")] = m.GetInt("count_mysql_snapshots")
		return nil
	})
Cleanup:

	if err != nil {
		log.Errore(err)
	}
	return res, err
}

// For the given list of instances, fill in extra data acquired from agents
// At current this is the number of snapshots.
// This isn't too pretty; it's a push-into-instance-data-that-belongs-to-agent thing.
// Originally the need was to visually present the number of snapshots per host on the web/cluster page, which
// indeed proves to be useful in our experience.
func PopulateInstancesAgents(instances [](*Instance)) error {
	if len(instances) == 0 {
		return nil
	}
	hostnames := []string{}
	for _, instance := range instances {
		hostnames = append(hostnames, instance.Key.Hostname)
	}
	agentsCountMySQLSnapshots, err := ReadCountMySQLSnapshots(hostnames)
	if err != nil {
		return err
	}
	for _, instance := range instances {
		if count, ok := agentsCountMySQLSnapshots[instance.Key.Hostname]; ok {
			instance.CountMySQLSnapshots = count
		}
	}

	return nil
}

// ReadClusters reads names of all known clusters
func ReadClusters() ([]string, error) {
	clusterNames := []string{}

	db, err := db.OpenOrchestrator()
	if err != nil {
		return clusterNames, log.Errore(err)
	}

	query := fmt.Sprintf(`
		select 
			cluster_name
		from 
			database_instance 
		group by
			cluster_name`)

	err = sqlutils.QueryRowsMap(db, query, func(m sqlutils.RowMap) error {
		clusterNames = append(clusterNames, m.GetString("cluster_name"))
		return nil
	})

	return clusterNames, err
}

// ReadClusterInfo reads some info about a given cluster
func ReadClusterInfo(clusterName string) (*ClusterInfo, error) {
	clusterInfo := &ClusterInfo{}

	db, err := db.OpenOrchestrator()
	if err != nil {
		return clusterInfo, log.Errore(err)
	}

	query := fmt.Sprintf(`
		select 
			cluster_name,
			count(*) as count_instances
		from 
			database_instance 
		where
			cluster_name='%s'
		group by
			cluster_name`, clusterName)

	err = sqlutils.QueryRowsMap(db, query, func(m sqlutils.RowMap) error {
		clusterInfo.ClusterName = m.GetString("cluster_name")
		clusterInfo.CountInstances = m.GetUint("count_instances")
		ApplyClusterAlias(clusterInfo)
		return nil
	})

	return clusterInfo, err
}

// ReadClustersInfo reads names of all known clusters and some aggregated info
func ReadClustersInfo() ([]ClusterInfo, error) {
	clusters := []ClusterInfo{}

	db, err := db.OpenOrchestrator()
	if err != nil {
		return clusters, log.Errore(err)
	}

	query := fmt.Sprintf(`
		select 
			cluster_name,
			count(*) as count_instances
		from 
			database_instance 
		group by
			cluster_name`)

	err = sqlutils.QueryRowsMap(db, query, func(m sqlutils.RowMap) error {
		clusterInfo := ClusterInfo{
			ClusterName:    m.GetString("cluster_name"),
			CountInstances: m.GetUint("count_instances"),
		}
		ApplyClusterAlias(&clusterInfo)

		clusters = append(clusters, clusterInfo)
		return nil
	})

	return clusters, err
}

// ReadOutdatedInstanceKeys reads and returns keys for all instances that are not up to date (i.e.
// pre-configured time has passed since they were last cheked)
// But we also check for the case where an attempt at instance checking has been made, that hasn't
// resulted in an actual check! This can happen when TCP/IP connections are hung, in which case the "check"
// never returns. In such case we multiply interval by a factor, so as not to open too many connections on
// the instance.
func ReadOutdatedInstanceKeys() ([]InstanceKey, error) {
	res := []InstanceKey{}
	query := fmt.Sprintf(`
		select 
			hostname, port 
		from 
			database_instance 
		where
			if (
				last_attempted_check <= last_checked,
				last_checked < now() - interval %d second,
				last_checked < now() - interval (%d * 20) second
			)
			`,
		config.Config.InstancePollSeconds, config.Config.InstancePollSeconds)
	db, err := db.OpenOrchestrator()
	if err != nil {
		goto Cleanup
	}

	err = sqlutils.QueryRowsMap(db, query, func(m sqlutils.RowMap) error {
		instanceKey, merr := NewInstanceKeyFromStrings(m.GetString("hostname"), m.GetString("port"))
		if merr != nil {
			log.Errore(merr)
		} else {
			res = append(res, *instanceKey)
		}
		// We don;t return an error because we want to keep filling the outdated instances list.
		return nil
	})
Cleanup:

	if err != nil {
		log.Errore(err)
	}
	return res, err

}

// writeInstance stores an instance in the orchestrator backend
func writeInstance(instance *Instance, instanceWasActuallyFound bool, lastError error) error {

	writeFunc := func() error {
		db, err := db.OpenOrchestrator()
		if err != nil {
			return log.Errore(err)
		}

		insertIgnore := ""
		onDuplicateKeyUpdate := ""
		if instanceWasActuallyFound {
			// Data is real
			onDuplicateKeyUpdate = `
				on duplicate key update
	        		last_checked=VALUES(last_checked),
	        		last_attempted_check=VALUES(last_attempted_check),
	        		server_id=VALUES(server_id),
					version=VALUES(version),
					read_only=VALUES(read_only),
					binlog_format=VALUES(binlog_format),
					log_bin=VALUES(log_bin),
					log_slave_updates=VALUES(log_slave_updates),
					binary_log_file=VALUES(binary_log_file),
					binary_log_pos=VALUES(binary_log_pos),
					master_host=VALUES(master_host),
					master_port=VALUES(master_port),
					slave_sql_running=VALUES(slave_sql_running),
					slave_io_running=VALUES(slave_io_running),
					oracle_gtid=VALUES(oracle_gtid),
					mariadb_gtid=VALUES(mariadb_gtid),
					master_log_file=VALUES(master_log_file),
					read_master_log_pos=VALUES(read_master_log_pos),
					relay_master_log_file=VALUES(relay_master_log_file),
					exec_master_log_pos=VALUES(exec_master_log_pos),
					relay_log_file=VALUES(relay_log_file),
					relay_log_pos=VALUES(relay_log_pos),
					last_sql_error=VALUES(last_sql_error),
					last_io_error=VALUES(last_io_error),
					seconds_behind_master=VALUES(seconds_behind_master),
					slave_lag_seconds=VALUES(slave_lag_seconds),
					num_slave_hosts=VALUES(num_slave_hosts),
					slave_hosts=VALUES(slave_hosts),
					cluster_name=VALUES(cluster_name),
					replication_depth=VALUES(replication_depth)			
				`
		} else {
			// Scenario: some slave reported a master of his; but the master cannot be contacted.
			// We might still want to have that master written down
			// Use with caution
			insertIgnore = `ignore`
		}
		insertQuery := fmt.Sprintf(`
        	insert %s into database_instance (
        		hostname,
        		port,
        		last_checked,
        		last_attempted_check,
        		server_id,
				version,
				read_only,
				binlog_format,
				log_bin,
				log_slave_updates,
				binary_log_file,
				binary_log_pos,
				master_host,
				master_port,
				slave_sql_running,
				slave_io_running,
				oracle_gtid,
				mariadb_gtid,
				pseudo_gtid,
				master_log_file,
				read_master_log_pos,
				relay_master_log_file,
				exec_master_log_pos,
				relay_log_file,
				relay_log_pos,
				last_sql_error,
				last_io_error,
				seconds_behind_master,
				slave_lag_seconds,
				num_slave_hosts,
				slave_hosts,
				cluster_name,
				replication_depth
			) values (?, ?, NOW(), NOW(), ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			%s
			`, insertIgnore, onDuplicateKeyUpdate)

		_, err = sqlutils.Exec(db, insertQuery,
			instance.Key.Hostname,
			instance.Key.Port,
			instance.ServerID,
			instance.Version,
			instance.ReadOnly,
			instance.Binlog_format,
			instance.LogBinEnabled,
			instance.LogSlaveUpdatesEnabled,
			instance.SelfBinlogCoordinates.LogFile,
			instance.SelfBinlogCoordinates.LogPos,
			instance.MasterKey.Hostname,
			instance.MasterKey.Port,
			instance.Slave_SQL_Running,
			instance.Slave_IO_Running,
			instance.UsingOracleGTID,
			instance.UsingMariaDBGTID,
			instance.UsingPseudoGTID,
			instance.ReadBinlogCoordinates.LogFile,
			instance.ReadBinlogCoordinates.LogPos,
			instance.ExecBinlogCoordinates.LogFile,
			instance.ExecBinlogCoordinates.LogPos,
			instance.RelaylogCoordinates.LogFile,
			instance.RelaylogCoordinates.LogPos,
			instance.LastSQLError,
			instance.LastIOError,
			instance.SecondsBehindMaster,
			instance.SlaveLagSeconds,
			len(instance.SlaveHosts),
			instance.GetSlaveHostsAsJson(),
			instance.ClusterName,
			instance.ReplicationDepth,
		)
		if err != nil {
			return log.Errore(err)
		}

		if instanceWasActuallyFound && lastError == nil {
			sqlutils.Exec(db, `
        	update database_instance set last_seen = NOW() where hostname=? and port=?
        	`, instance.Key.Hostname, instance.Key.Port,
			)
		} else {
			log.Debugf("writeInstance: will not update database_instance due to error: %+v", lastError)
		}
		return nil
	}
	return ExecDBWriteFunc(writeFunc)
}

// UpdateInstanceLastChecked updates the last_check timestamp in the orchestrator backed database
// for a given instance
func UpdateInstanceLastChecked(instanceKey *InstanceKey) error {
	writeFunc := func() error {
		db, err := db.OpenOrchestrator()
		if err != nil {
			return log.Errore(err)
		}

		_, err = sqlutils.Exec(db, `
        	update 
        		database_instance 
        	set
        		last_checked = NOW()
			where 
				hostname = ?
				and port = ?`,
			instanceKey.Hostname,
			instanceKey.Port,
		)
		if err != nil {
			return log.Errore(err)
		}

		return nil
	}
	return ExecDBWriteFunc(writeFunc)
}

// UpdateInstanceLastAttemptedCheck updates the last_attempted_check timestamp in the orchestrator backed database
// for a given instance.
// This is used as a failsafe mechanism in case access to the instance gets hung (it happens), in which case
// the entire ReadTopology gets stuck (and no, connection timeout nor driver timeouts don't help. Don't look at me,
// the world is a harsh place to live in).
// And so we make sure to note down *before* we even attempt to access the instance; and this raises a red flag when we
// wish to access the instance again: if last_attempted_check is *newer* than last_checked, that's bad news and means
// we have a "hanging" issue.
func UpdateInstanceLastAttemptedCheck(instanceKey *InstanceKey) error {
	writeFunc := func() error {
		db, err := db.OpenOrchestrator()
		if err != nil {
			return log.Errore(err)
		}

		_, err = sqlutils.Exec(db, `
        	update 
        		database_instance 
        	set
        		last_attempted_check = NOW()
			where 
				hostname = ?
				and port = ?`,
			instanceKey.Hostname,
			instanceKey.Port,
		)
		if err != nil {
			return log.Errore(err)
		}

		return nil
	}
	return ExecDBWriteFunc(writeFunc)
}

// ForgetInstance removes an instance entry from the orchestrator backed database.
// It may be auto-rediscovered through topology or requested for discovery by multiple means.
func ForgetInstance(instanceKey *InstanceKey) error {
	db, err := db.OpenOrchestrator()
	if err != nil {
		return log.Errore(err)
	}

	_, err = sqlutils.Exec(db, `
			delete 
				from database_instance 
			where 
				hostname = ? and port = ?`,
		instanceKey.Hostname,
		instanceKey.Port,
	)
	AuditOperation("forget", instanceKey, "")
	return err
}

// ForgetLongUnseenInstances will remove entries of all instacnes that have long since been last seen.
func ForgetLongUnseenInstances() error {
	db, err := db.OpenOrchestrator()
	if err != nil {
		return log.Errore(err)
	}

	sqlResult, err := sqlutils.Exec(db, `
			delete 
				from database_instance 
			where 
				last_seen < NOW() - interval ? hour`,
		config.Config.UnseenInstanceForgetHours,
	)
	if err != nil {
		return log.Errore(err)
	}
	rows, err := sqlResult.RowsAffected()
	if err != nil {
		return log.Errore(err)
	}
	AuditOperation("forget-unseen", nil, fmt.Sprintf("Forgotten instances: %d", rows))
	return err
}

// RefreshTopologyInstance will synchronuously re-read topology instance
func RefreshTopologyInstance(instanceKey *InstanceKey) (*Instance, error) {
	_, err := ReadTopologyInstance(instanceKey)
	if err != nil {
		return nil, err
	}

	inst, found, err := ReadInstance(instanceKey)
	if err != nil || !found {
		return nil, err
	}

	return inst, nil
}

// RefreshTopologyInstances will do a blocking (though concurrent) refresh of all given instances
func RefreshTopologyInstances(instances [](*Instance)) {
	// use concurrency but wait for all to complete
	barrier := make(chan InstanceKey)
	for _, instance := range instances {
		instance := instance
		go func() {
			// Wait your turn to read a slave
			ExecuteOnTopology(func() {
				log.Debugf("... reading instance: %+v", instance.Key)
				ReadTopologyInstance(&instance.Key)
			})
			// Signal compelted slave
			barrier <- instance.Key
		}()
	}
	for _ = range instances {
		<-barrier
	}
}

// RefreshInstanceSlaveHosts is a workaround for a bug in MySQL where
// SHOW SLAVE HOSTS continues to present old, long disconnected slaves.
// It turns out issuing a couple FLUSH commands mitigates the problem.
func RefreshInstanceSlaveHosts(instanceKey *InstanceKey) (*Instance, error) {
	_, _ = ExecInstance(instanceKey, `flush error logs`)
	_, _ = ExecInstance(instanceKey, `flush error logs`)

	instance, err := ReadTopologyInstance(instanceKey)
	return instance, err
}

// StopSlaveNicely stops a slave such that SQL_thread and IO_thread are aligned (i.e.
// SQL_thread consumes all relay log entries)
// It will actually START the sql_thread even if the slave is completely stopped.
func StopSlaveNicely(instanceKey *InstanceKey, timeout time.Duration) (*Instance, error) {
	instance, err := ReadTopologyInstance(instanceKey)
	if err != nil {
		return instance, log.Errore(err)
	}

	if !instance.IsSlave() {
		return instance, errors.New(fmt.Sprintf("instance is not a slave: %+v", instanceKey))
	}

	_, err = ExecInstance(instanceKey, `stop slave io_thread`)
	_, err = ExecInstance(instanceKey, `start slave sql_thread`)

	startTime := time.Now()
	for upToDate := false; !upToDate; {
		if timeout > 0 && time.Since(startTime) >= timeout {
			// timeout
			return nil, errors.New(fmt.Sprintf("StopSlaveNicely timeout on %+v", *instanceKey))
		}
		instance, err = ReadTopologyInstance(instanceKey)
		if err != nil {
			return instance, log.Errore(err)
		}

		if instance.SQLThreadUpToDate() {
			upToDate = true
		} else {
			time.Sleep(SQLThreadPollDuration)
		}
	}
	_, err = ExecInstance(instanceKey, `stop slave`)
	if err != nil {
		return instance, log.Errore(err)
	}

	instance, err = ReadTopologyInstance(instanceKey)
	return instance, err
}

// StopSlavesNicely will attemt to stop all given slaves nicely, up to timeout
func StopSlavesNicely(slaves [](*Instance), timeout time.Duration) {
	// use concurrency but wait for all to complete
	barrier := make(chan InstanceKey)
	for _, instance := range slaves {
		instance := instance
		go func() {
			// Wait your turn to read a slave
			ExecuteOnTopology(func() { StopSlaveNicely(&instance.Key, timeout) })
			// Signal compelted slave
			barrier <- instance.Key
		}()
	}
	for _ = range slaves {
		<-barrier
	}
}

// StopSlave stops replication on a given instance
func StopSlave(instanceKey *InstanceKey) (*Instance, error) {
	instance, err := ReadTopologyInstance(instanceKey)
	if err != nil {
		return instance, log.Errore(err)
	}

	if !instance.IsSlave() {
		return instance, errors.New(fmt.Sprintf("instance is not a slave: %+v", instanceKey))
	}
	_, err = ExecInstance(instanceKey, `stop slave`)
	if err != nil {
		return instance, log.Errore(err)
	}
	instance, err = ReadTopologyInstance(instanceKey)

	log.Infof("Stopped slave on %+v, Self:%+v, Exec:%+v", *instanceKey, instance.SelfBinlogCoordinates, instance.ExecBinlogCoordinates)
	return instance, err
}

// StartSlave starts replication on a given instance
func StartSlave(instanceKey *InstanceKey) (*Instance, error) {
	instance, err := ReadTopologyInstance(instanceKey)
	if err != nil {
		return instance, log.Errore(err)
	}

	if !instance.IsSlave() {
		return instance, errors.New(fmt.Sprintf("instance is not a slave: %+v", instanceKey))
	}

	_, err = ExecInstance(instanceKey, `start slave`)
	if err != nil {
		return instance, log.Errore(err)
	}
	log.Infof("Started slave on %+v", instanceKey)
	if config.Config.SlaveStartPostWaitMilliseconds > 0 {
		time.Sleep(time.Duration(config.Config.SlaveStartPostWaitMilliseconds) * time.Millisecond)
	}

	instance, err = ReadTopologyInstance(instanceKey)
	return instance, err
}

// StartSlaves will do concurrent start-slave
func StartSlaves(slaves [](*Instance)) {
	// use concurrency but wait for all to complete
	barrier := make(chan InstanceKey)
	for _, instance := range slaves {
		instance := instance
		go func() {
			// Wait your turn to read a slave
			ExecuteOnTopology(func() { StartSlave(&instance.Key) })
			// Signal compelted slave
			barrier <- instance.Key
		}()
	}
	for _ = range slaves {
		<-barrier
	}
}

// StartSlaveUntilMasterCoordinates issuesa START SLAVE UNTIL... statement on given instance
func StartSlaveUntilMasterCoordinates(instanceKey *InstanceKey, masterCoordinates *BinlogCoordinates) (*Instance, error) {
	instance, err := ReadTopologyInstance(instanceKey)
	if err != nil {
		return instance, log.Errore(err)
	}

	if !instance.IsSlave() {
		return instance, errors.New(fmt.Sprintf("instance is not a slave: %+v", instanceKey))
	}
	if instance.SlaveRunning() {
		return instance, errors.New(fmt.Sprintf("slave already running: %+v", instanceKey))
	}

	log.Infof("Will start slave on %+v until coordinates: %+v", instanceKey, masterCoordinates)

	_, err = ExecInstance(instanceKey, fmt.Sprintf("start slave until master_log_file='%s', master_log_pos=%d",
		masterCoordinates.LogFile, masterCoordinates.LogPos))
	if err != nil {
		return instance, log.Errore(err)
	}

	for upToDate := false; !upToDate; {
		instance, err = ReadTopologyInstance(instanceKey)
		if err != nil {
			return instance, log.Errore(err)
		}

		switch {
		case instance.ExecBinlogCoordinates.SmallerThan(masterCoordinates):
			time.Sleep(SQLThreadPollDuration)
		case instance.ExecBinlogCoordinates.Equals(masterCoordinates):
			upToDate = true
		case masterCoordinates.SmallerThan(&instance.ExecBinlogCoordinates):
			return instance, errors.New(fmt.Sprintf("Start SLAVE UNTIL is past coordinates: %+v", instanceKey))
		}
	}

	instance, err = StopSlave(instanceKey)
	if err != nil {
		return instance, log.Errore(err)
	}

	return instance, err
}

// ChangeMasterTo changes the given instance's master according to given input.
func ChangeMasterTo(instanceKey *InstanceKey, masterKey *InstanceKey, masterBinlogCoordinates *BinlogCoordinates) (*Instance, error) {
	instance, err := ReadTopologyInstance(instanceKey)
	if err != nil {
		return instance, log.Errore(err)
	}

	if instance.SlaveRunning() {
		return instance, errors.New(fmt.Sprintf("Cannot change master on: %+v because slave is running", instanceKey))
	}

	_, err = ExecInstance(instanceKey, fmt.Sprintf("change master to master_host='%s', master_port=%d, master_log_file='%s', master_log_pos=%d",
		masterKey.Hostname, masterKey.Port, masterBinlogCoordinates.LogFile, masterBinlogCoordinates.LogPos))
	if err != nil {
		return instance, log.Errore(err)
	}
	log.Infof("Changed master on %+v to: %+v, %+v", instanceKey, masterKey, masterBinlogCoordinates)

	instance, err = ReadTopologyInstance(instanceKey)
	return instance, err
}

// ResetSlave resets a slave, breaking the replication
func ResetSlave(instanceKey *InstanceKey) (*Instance, error) {
	instance, err := ReadTopologyInstance(instanceKey)
	if err != nil {
		return instance, log.Errore(err)
	}

	if instance.SlaveRunning() {
		return instance, errors.New(fmt.Sprintf("Cannot reset slave on: %+v because slave is running", instanceKey))
	}

	// MySQL's RESET SLAVE is done correctly; however SHOW SLAVE STATUS still returns old hostnames etc
	// and only resets till after next restart. This leads to orchestrator still thinking the instance replicates
	// from old host. We therefore forcibly modify the hostname.
	_, err = ExecInstance(instanceKey, `change master to master_host='_'`)
	if err != nil {
		return instance, log.Errore(err)
	}
	_, err = ExecInstance(instanceKey, `reset slave`)
	if err != nil {
		return instance, log.Errore(err)
	}
	log.Infof("Reset slave %+v", instanceKey)

	instance, err = ReadTopologyInstance(instanceKey)
	return instance, err
}

// DetachSlave detaches a slave from replication; forcibly corrupting the binlog coordinates (though in such way
// that is reversible)
func DetachSlave(instanceKey *InstanceKey) (*Instance, error) {
	instance, err := ReadTopologyInstance(instanceKey)
	if err != nil {
		return instance, log.Errore(err)
	}

	if instance.SlaveRunning() {
		return instance, errors.New(fmt.Sprintf("Cannot detach slave on: %+v because slave is running", instanceKey))
	}

	detachedCoordinatesSubmatch := detachPattern.FindStringSubmatch(instance.ExecBinlogCoordinates.LogFile)
	if len(detachedCoordinatesSubmatch) != 0 {
		return instance, errors.New(fmt.Sprintf("Cannot (need not) detach slave on: %+v because slave is already detached", instanceKey))
	}

	detachedCoordinates := BinlogCoordinates{LogFile: fmt.Sprintf("//%s:%d", instance.ExecBinlogCoordinates.LogFile, instance.ExecBinlogCoordinates.LogPos), LogPos: instance.ExecBinlogCoordinates.LogPos}
	// Encode the current coordinates within the log file name, in such way that replication is broken, but info can still be resurrected
	_, err = ExecInstance(instanceKey, fmt.Sprintf(`change master to master_log_file='%s', master_log_pos=%d`, detachedCoordinates.LogFile, detachedCoordinates.LogPos))
	if err != nil {
		return instance, log.Errore(err)
	}

	log.Infof("Detach slave %+v", instanceKey)

	instance, err = ReadTopologyInstance(instanceKey)
	return instance, err
}

// ReattachSlave restores a detahced slave back into replication
func ReattachSlave(instanceKey *InstanceKey) (*Instance, error) {
	instance, err := ReadTopologyInstance(instanceKey)
	if err != nil {
		return instance, log.Errore(err)
	}

	if instance.SlaveRunning() {
		return instance, errors.New(fmt.Sprintf("Cannot (need not) reattach slave on: %+v because slave is running", instanceKey))
	}

	detachedCoordinatesSubmatch := detachPattern.FindStringSubmatch(instance.ExecBinlogCoordinates.LogFile)
	if len(detachedCoordinatesSubmatch) == 0 {
		return instance, errors.New(fmt.Sprintf("Cannot reattach slave on: %+v because slave is not detached", instanceKey))
	}

	_, err = ExecInstance(instanceKey, fmt.Sprintf(`change master to master_log_file='%s', master_log_pos=%s`, detachedCoordinatesSubmatch[1], detachedCoordinatesSubmatch[2]))
	if err != nil {
		return instance, log.Errore(err)
	}

	log.Infof("Reattach slave %+v", instanceKey)

	instance, err = ReadTopologyInstance(instanceKey)
	return instance, err
}

// MasterPosWait issues a MASTER_POS_WAIT() an given instance according to given coordinates.
func MasterPosWait(instanceKey *InstanceKey, binlogCoordinates *BinlogCoordinates) (*Instance, error) {
	instance, err := ReadTopologyInstance(instanceKey)
	if err != nil {
		return instance, log.Errore(err)
	}

	_, err = ExecInstance(instanceKey, fmt.Sprintf("select master_pos_wait('%s', %d)",
		binlogCoordinates.LogFile, binlogCoordinates.LogPos))
	if err != nil {
		return instance, log.Errore(err)
	}
	log.Infof("Instance %+v has reached coordinates: %+v", instanceKey, binlogCoordinates)

	instance, err = ReadTopologyInstance(instanceKey)
	return instance, err
}

// SetReadOnly sets or clears the instance's global read_only variable
func SetReadOnly(instanceKey *InstanceKey, readOnly bool) (*Instance, error) {
	instance, err := ReadTopologyInstance(instanceKey)
	if err != nil {
		return instance, log.Errore(err)
	}

	_, err = ExecInstance(instanceKey, fmt.Sprintf("set global read_only = %t", readOnly))
	if err != nil {
		return instance, log.Errore(err)
	}
	instance, err = ReadTopologyInstance(instanceKey)

	log.Infof("instance %+v read_only: %t", instanceKey, readOnly)
	AuditOperation("read-only", instanceKey, fmt.Sprintf("set as %t", readOnly))

	return instance, err
}

// KillQuery stops replication on a given instance
func KillQuery(instanceKey *InstanceKey, process int64) (*Instance, error) {
	instance, err := ReadTopologyInstance(instanceKey)
	if err != nil {
		return instance, log.Errore(err)
	}

	_, err = ExecInstance(instanceKey, fmt.Sprintf(`kill query %d`, process))
	if err != nil {
		return instance, log.Errore(err)
	}

	instance, err = ReadTopologyInstance(instanceKey)
	if err != nil {
		return instance, log.Errore(err)
	}

	log.Infof("Killed query on %+v", *instanceKey)
	AuditOperation("kill-query", instanceKey, fmt.Sprintf("Killed query %d", process))
	return instance, err
}
