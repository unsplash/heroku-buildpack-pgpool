package main

import (
	"crypto/md5"
	"fmt"
	"github.com/chonla/format"
	"log"
	"net"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"sync"
	"syscall"
)

func main() {
	if os.Getenv("PGPOOL_ENABLED") == "0" {
		cmd, err := exec.LookPath(os.Args[1])

		if err != nil {
			log.Fatal(err)
		}

		if err := syscall.Exec(cmd, os.Args[1:], os.Environ()); err != nil {
			log.Fatal(err)
		}
	}

	configure()

	var wg sync.WaitGroup
	sigterm := make(chan os.Signal, 1)

	signal.Ignore(syscall.SIGINT)
	signal.Notify(sigterm, syscall.SIGTERM)

	pgpool := run(true, "/app/.apt/usr/sbin/pgpool", "-n", "-f", "/app/vendor/pgpool/pgpool.conf", "-a", "/app/vendor/pgpool/pool_hba.conf")

	defer pgpool.Process.Kill()
	wg.Add(1)

	app := run(false, os.Args[1], os.Args[2:]...)
	wg.Add(1)

	go func() {
		<-sigterm
		app.Process.Signal(syscall.SIGTERM)
	}()

	go func() {
		err := app.Wait()

		if err != nil {
			log.Println("app:", err)
		}

		if pgpool.Process != nil {
			pgpool.Process.Signal(syscall.SIGTERM)
		}

		wg.Done()
	}()

	go func() {
		err := pgpool.Wait()

		if err != nil {
			log.Println("pgpool:", err)
		}

		if app.Process != nil {
			app.Process.Signal(syscall.SIGTERM)
		}

		wg.Done()
	}()

	wg.Wait()
}

func configure() {
	configurePgpoolConf()
	configurePoolPasswd()
}

func configurePgpoolConf() {
	var pgpoolConf []byte

	pgpoolConf = append(pgpoolConf, `
		socket_dir = '/tmp'
		pcp_socket_dir = '/tmp'
		ssl = on
		pid_file_name = '/tmp/pgpool.pid'
		logdir = '/tmp'
	`...)

	for i, postgresUrl := range postgresUrls() {
		host, port, _ := net.SplitHostPort(postgresUrl.Host)
		user := postgresUrl.User.Username()
		database := postgresUrl.Path[1:]

		if i == 0 {
			statementLoadBalance := os.Getenv("PGPOOL_STATEMENT_LOAD_BALANCE")

			if statementLoadBalance == "" {
				statementLoadBalance = "off"
			}

			maxPool := os.Getenv("PGPOOL_MAX_POOL")

			if maxPool == "" {
				maxPool = "4"
			}

			numChildren := os.Getenv("PGPOOL_NUM_INIT_CHILDREN")

			if numChildren == "" {
				numChildren = "32"
			}

			var params = map[string]interface{}{
				"user":         user,
				"database":     database,
				"load_balance": statementLoadBalance,
				"max_pool":     maxPool,
				"num_children": numChildren,
			}

			pgpoolConf = append(pgpoolConf, format.Sprintf(`
        backend_clustering_mode       = 'streaming_replication'
        disable_load_balance_on_write = 'transaction'

        load_balance_mode = 'on'
        enable_pool_hba = 'on'

        max_pool = %<max_pool>s
        num_init_children = %<num_children>s

        failover_on_backend_shutdown = 'off'
        failover_on_backend_error = 'off'

        sr_check_user     = '%<user>s'
        sr_check_database = '%<database>s'
        sr_check_period   = 30

        health_check_user     = '%<user>s'
        health_check_database = '%<database>s'

        health_check_period      = 5
        health_check_timeout     = 30
        health_check_max_retries = 9999
        health_check_retry_delay = 1

        statement_level_load_balance = '%<load_balance>s'
        allow_sql_comments = true
      `, params)...)

			if os.Getenv("PGPOOL_DEBUG") != "" {
				pgpoolConf = append(pgpoolConf, `
          log_destination        = 'stderr'
          log_statement          = 'on'
          log_per_node_statement = 'on'
        `...)
			}
		}

		weight := os.Getenv(fmt.Sprintf("PGPOOL_BACKEND_NODE_%d_WEIGHT", i))

		if weight == "" {
			weight = "1"
		}

		flag := os.Getenv(fmt.Sprintf("PGPOOL_BACKEND_NODE_%d_FLAG", i))

		if flag == "" {
			flag = "ALLOW_TO_FAILOVER"
		}

		var data = map[string]interface{}{
			"index":  i,
			"host":   host,
			"port":   port,
			"weight": weight,
			"flag":   flag,
		}

		pgpoolConf = append(pgpoolConf, format.Sprintf(`
			backend_hostname%<index>d         = '%<host>s'
			backend_application_name%<index>d = 'server%<host>s'
			backend_port%<index>d             = %<port>s
			backend_weight%<index>d           = %<weight>s
			backend_flag%<index>d             = '%<flag>s'
			backend_data_directory%<index>d   = 'data%<index>d'
		`, data)...)
	}

	// This is helpful to debug the file
	configTarget := os.Getenv("PGPOOL_CONFIG_TARGET")
	hbaTarget := os.Getenv("PGPOOL_HBA_TARGET")

	if configTarget == "" {
		configTarget = "/app/vendor/pgpool/pgpool.conf"
	}

	if hbaTarget == "" {
		hbaTarget = "/app/vendor/pgpool/pool_hba.conf"
	}

	hba := `
  local   all             all                                     trust
  host    all             all             127.0.0.1/32            trust
  `

	err := os.WriteFile(hbaTarget, []byte(hba), 0600)

	if err != nil {
		log.Fatal(err)
	}

	err = os.WriteFile(configTarget, pgpoolConf, 0600)

	if err != nil {
		log.Fatal(err)
	}
}

func configurePoolPasswd() {
	poolPasswd := ""

	for _, postgresUrl := range postgresUrls() {
		user := postgresUrl.User.Username()
		password, _ := postgresUrl.User.Password()
		poolPasswd += fmt.Sprintf("%s:md5%x\n", user, md5.Sum([]byte(password+user)))
	}

	err := os.WriteFile("/app/vendor/pgpool/pool_passwd", []byte(poolPasswd), 0600)

	if err != nil {
		log.Fatal(err)
	}
}

func postgresUrls() []*url.URL {
	pgpoolUrls := strings.Split(os.Getenv("PGPOOL_URLS"), " ")

	if len(pgpoolUrls) == 0 {
		log.Fatal("PGPOOL_URLS is not set")
	}

	postgresUrls := make([]*url.URL, len(pgpoolUrls))

	for i, pgpoolUrl := range pgpoolUrls {
		postgresUrl := os.Getenv(pgpoolUrl)

		if postgresUrl == "" {
			log.Fatal(pgpoolUrl + " is not set")
		}

		postgresUrlUrl, err := url.Parse(postgresUrl)
		if err != nil {
			log.Println(err)
			log.Fatal(pgpoolUrl + " is invalid")
		}

		postgresUrls[i] = postgresUrlUrl
	}

	return postgresUrls
}

func databaseUrl() string {
	postgresUrl := postgresUrls()[0]

	user := postgresUrl.User.Username()
	password, _ := postgresUrl.User.Password()
	database := postgresUrl.Path[1:]

	return fmt.Sprintf("postgres://%s:%s@localhost:9999/%s", user, password, database)
}

func run(pgpool bool, command string, args ...string) *exec.Cmd {
	cmd := exec.Command(command, args...)

	if pgpool {
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	} else {
		cmd.Stdin = os.Stdin
		cmd.Env = append(os.Environ(), fmt.Sprintf("DATABASE_URL=%s", databaseUrl()))
	}

	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		log.Fatal(err)
	}

	return cmd
}
