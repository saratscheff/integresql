package manager

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"runtime/trace"
	"sync"

	"github.com/allaboutapps/integresql/pkg/db"
	"github.com/allaboutapps/integresql/pkg/pool"
	"github.com/allaboutapps/integresql/pkg/templates"
	"github.com/lib/pq"
)

var (
	ErrManagerNotReady            = errors.New("manager not ready")
	ErrTemplateAlreadyInitialized = errors.New("template is already initialized")
	ErrTemplateNotFound           = errors.New("template not found")
	ErrInvalidTemplateState       = errors.New("unexpected template state")
	ErrTestNotFound               = errors.New("test database not found")
)

type Manager struct {
	config        ManagerConfig
	db            *sql.DB
	templates     map[string]*TemplateDatabase
	templateMutex sync.RWMutex
	wg            sync.WaitGroup

	closeChan  chan bool
	templatesX *templates.Collection
	pool       *pool.DBPool
}

func New(config ManagerConfig) *Manager {
	m := &Manager{
		config:     config,
		db:         nil,
		templates:  map[string]*TemplateDatabase{},
		wg:         sync.WaitGroup{},
		templatesX: templates.NewCollection(),
		pool:       pool.NewDBPool(config.TestDatabaseMaxPoolSize),
	}

	if len(m.config.TestDatabaseOwner) == 0 {
		m.config.TestDatabaseOwner = m.config.ManagerDatabaseConfig.Username
	}

	if len(m.config.TestDatabaseOwnerPassword) == 0 {
		m.config.TestDatabaseOwnerPassword = m.config.ManagerDatabaseConfig.Password
	}

	if m.config.TestDatabaseInitialPoolSize > m.config.TestDatabaseMaxPoolSize && m.config.TestDatabaseMaxPoolSize > 0 {
		m.config.TestDatabaseInitialPoolSize = m.config.TestDatabaseMaxPoolSize
	}

	return m
}

func DefaultFromEnv() *Manager {
	return New(DefaultManagerConfigFromEnv())
}

func (m *Manager) Connect(ctx context.Context) error {
	if m.db != nil {
		return errors.New("manager is already connected")
	}

	db, err := sql.Open("postgres", m.config.ManagerDatabaseConfig.ConnectionString())
	if err != nil {
		return err
	}

	if err := db.PingContext(ctx); err != nil {
		return err
	}

	m.db = db

	return nil
}

func (m *Manager) Disconnect(ctx context.Context, ignoreCloseError bool) error {
	if m.db == nil {
		return errors.New("manager is not connected")
	}

	m.closeChan <- true

	c := make(chan struct{})
	go func() {
		defer close(c)
		m.wg.Wait()
	}()

	select {
	case <-c:
	case <-ctx.Done():
	}

	if err := m.db.Close(); err != nil && !ignoreCloseError {
		return err
	}

	m.db = nil

	return nil
}

func (m *Manager) Reconnect(ctx context.Context, ignoreDisconnectError bool) error {
	if err := m.Disconnect(ctx, ignoreDisconnectError); err != nil && !ignoreDisconnectError {
		return err
	}

	return m.Connect(ctx)
}

func (m *Manager) Ready() bool {
	return m.db != nil
}

func (m *Manager) Initialize(ctx context.Context) error {
	if !m.Ready() {
		if err := m.Connect(ctx); err != nil {
			return err
		}
	}

	rows, err := m.db.QueryContext(ctx, "SELECT datname FROM pg_database WHERE datname LIKE $1", fmt.Sprintf("%s_%s_%%", m.config.DatabasePrefix, m.config.TestDatabasePrefix))
	if err != nil {
		return err
	}
	defer rows.Close()

	for rows.Next() {
		var dbName string
		if err := rows.Scan(&dbName); err != nil {
			return err
		}

		if _, err := m.db.Exec(fmt.Sprintf("DROP DATABASE %s", pq.QuoteIdentifier(dbName))); err != nil {
			return err
		}
	}

	return nil
}

func (m *Manager) InitializeTemplateDatabase(ctx context.Context, hash string) (db.Database, error) {
	ctx, task := trace.NewTask(ctx, "initialize_template_db")
	defer task.End()

	if !m.Ready() {
		return db.Database{}, ErrManagerNotReady
	}

	dbName := m.makeTemplateDatabaseName(hash)
	templateConfig := db.DatabaseConfig{
		Host:     m.config.ManagerDatabaseConfig.Host,
		Port:     m.config.ManagerDatabaseConfig.Port,
		Username: m.config.ManagerDatabaseConfig.Username,
		Password: m.config.ManagerDatabaseConfig.Password,
		Database: dbName,
	}

	added, unlock := m.templatesX.Push(ctx, hash, templateConfig)
	// unlock template collection only after the template is actually initalized in the DB
	defer unlock()

	if !added {
		return db.Database{}, ErrTemplateAlreadyInitialized
	}

	reg := trace.StartRegion(ctx, "drop_and_create_db")
	if err := m.dropAndCreateDatabase(ctx, dbName, m.config.ManagerDatabaseConfig.Username, m.config.TemplateDatabaseTemplate); err != nil {
		m.templatesX.RemoveUnsafe(ctx, hash)

		return db.Database{}, err
	}
	reg.End()

	return db.Database{
		TemplateHash: hash,
		Config:       templateConfig,
	}, nil
}

func (m *Manager) DiscardTemplateDatabase(ctx context.Context, hash string) error {

	ctx, task := trace.NewTask(ctx, "discard_template_db")
	defer task.End()

	if !m.Ready() {
		return ErrManagerNotReady
	}

	template, found := m.templatesX.Pop(ctx, hash)
	dbName := template.Config.Database

	if !found {
		// even if a template is not found in the collection, it might still exist in the DB
		dbName = m.makeTemplateDatabaseName(hash)
		exists, err := m.checkDatabaseExists(ctx, dbName)
		if err != nil {
			return err
		}

		if !exists {
			return ErrTemplateNotFound
		}
	} else {
		template.SetState(ctx, templates.TemplateStateDiscarded)
	}

	return m.dropDatabase(ctx, dbName)
}

func (m *Manager) FinalizeTemplateDatabase(ctx context.Context, hash string) (db.Database, error) {
	ctx, task := trace.NewTask(ctx, "finalize_template_db")
	defer task.End()

	if !m.Ready() {
		return db.Database{}, ErrManagerNotReady
	}

	template, found := m.templatesX.Get(ctx, hash)
	if !found {
		return db.Database{}, ErrTemplateNotFound
	}

	state := template.GetState(ctx)

	// early bailout if we are already ready (multiple calls)
	if state == templates.TemplateStateReady {
		return template.Database, nil
	}

	// Disallow transition from discarded to ready
	if state == templates.TemplateStateDiscarded {
		return db.Database{}, ErrDatabaseDiscarded
	}

	template.SetState(ctx, templates.TemplateStateReady)

	m.addInitialTestDatabasesInBackground(template, m.config.TestDatabaseInitialPoolSize)

	return template.Database, nil
}

func (m *Manager) GetTestDatabase(ctx context.Context, hash string) (db.TestDatabase, error) {
	ctx, task := trace.NewTask(ctx, "get_test_db")
	defer task.End()

	if !m.Ready() {
		return db.TestDatabase{}, ErrManagerNotReady
	}

	template, found := m.templatesX.Get(ctx, hash)
	if !found {
		return db.TestDatabase{}, ErrTemplateNotFound
	}

	// if the template has been discarded/not initalized yet,
	// no DB should be returned, even if already in the pool
	state := template.WaitUntilReady(ctx, m.config.TestDatabaseWaitTimeout)
	if state != templates.TemplateStateReady {
		return db.TestDatabase{}, ErrInvalidTemplateState
	}

	testDB, dirty, err := m.pool.GetDB(ctx, template.TemplateHash)
	if err != nil {
		if !errors.Is(err, pool.ErrNoDBReady) {
			// internal error occurred, return directly
			return db.TestDatabase{}, err
		}

		// no DB is ready, we can try to add a new DB is pool is not full
		return m.createTestDatabaseFromTemplate(ctx, template)
	}

	// if no error occurred, a testDB has been found
	if !dirty {
		return testDB, nil
	}

	// clean it, if it's dirty, before returning it to the user
	return m.cleanTestDatabase(ctx, testDB, m.makeTemplateDatabaseName(testDB.TemplateHash))
}

func (m *Manager) ReturnTestDatabase(ctx context.Context, hash string, id int) error {
	if !m.Ready() {
		return ErrManagerNotReady
	}

	// check if the template exists and is 'ready'
	template, found := m.templatesX.Get(ctx, hash)
	if !found {
		return ErrTemplateNotFound
	}

	if template.WaitUntilReady(ctx, m.config.TestDatabaseWaitTimeout) != templates.TemplateStateReady {
		return ErrInvalidTemplateState
	}

	// template is ready, we can return the testDB to the pool
	return m.pool.ReturnTestDatabase(ctx, hash, id)
}

func (m *Manager) ClearTrackedTestDatabases(ctx context.Context, hash string) error {
	if !m.Ready() {
		return ErrManagerNotReady
	}

	removeFunc := func(testDB db.TestDatabase) error {
		return m.dropDatabase(ctx, testDB.Config.Database)
	}

	return m.pool.RemoveAllWithHash(ctx, hash, removeFunc)
}

func (m *Manager) ResetAllTracking(ctx context.Context) error {
	if !m.Ready() {
		return ErrManagerNotReady
	}

	// remove all templates to disallow any new test DB creation
	m.templatesX.RemoveAll(ctx)

	removeFunc := func(testDB db.TestDatabase) error {
		return m.dropDatabase(ctx, testDB.Config.Database)
	}
	if err := m.pool.RemoveAll(ctx, removeFunc); err != nil {
		return err
	}

	return nil
}

func (m *Manager) checkDatabaseExists(ctx context.Context, dbName string) (bool, error) {
	var exists bool
	if err := m.db.QueryRowContext(ctx, "SELECT 1 AS exists FROM pg_database WHERE datname = $1", dbName).Scan(&exists); err != nil {
		if err == sql.ErrNoRows {
			return false, nil
		}

		return false, err
	}

	return exists, nil
}

func (m *Manager) createDatabase(ctx context.Context, dbName string, owner string, template string) error {

	defer trace.StartRegion(ctx, "create_db").End()

	if _, err := m.db.ExecContext(ctx, fmt.Sprintf("CREATE DATABASE %s WITH OWNER %s TEMPLATE %s", pq.QuoteIdentifier(dbName), pq.QuoteIdentifier(owner), pq.QuoteIdentifier(template))); err != nil {
		return err
	}

	return nil
}

func (m *Manager) dropDatabase(ctx context.Context, dbName string) error {

	defer trace.StartRegion(ctx, "drop_db").End()

	if _, err := m.db.ExecContext(ctx, fmt.Sprintf("DROP DATABASE IF EXISTS %s", pq.QuoteIdentifier(dbName))); err != nil {
		return err
	}

	return nil
}

func (m *Manager) dropAndCreateDatabase(ctx context.Context, dbName string, owner string, template string) error {
	if err := m.dropDatabase(ctx, dbName); err != nil {
		return err
	}

	return m.createDatabase(ctx, dbName, owner, template)
}

// cleanTestDatabase recreates a dirty DB obtained from the pool.
// It is created according to the given template.
func (m *Manager) cleanTestDatabase(ctx context.Context, testDB db.TestDatabase, templateDBName string) (db.TestDatabase, error) {
	if err := m.dropAndCreateDatabase(ctx, testDB.Database.Config.Database, m.config.TestDatabaseOwner, templateDBName); err != nil {
		return db.TestDatabase{}, err
	}

	return testDB, nil
}

// createTestDatabaseFromTemplate adds a new test database in the pool (increasing its size) basing on the given template.
// It waits until the template is ready.
func (m *Manager) createTestDatabaseFromTemplate(ctx context.Context, template *templates.Template) (db.TestDatabase, error) {
	if template.WaitUntilReady(ctx, m.config.TestDatabaseWaitTimeout) != templates.TemplateStateReady {
		// if the state changed in the meantime, return
		return db.TestDatabase{}, ErrInvalidTemplateState
	}

	dbNamePrefix := m.makeTestDatabasePrefix(template.TemplateHash)
	testDB, err := m.pool.AddTestDatabase(ctx, template.Database, dbNamePrefix, func(testDB db.TestDatabase) error {
		return m.dropAndCreateDatabase(ctx, testDB.Database.Config.Database, m.config.TestDatabaseOwner, template.Config.Database)
	})

	if err != nil {
		return db.TestDatabase{}, err
	}

	return testDB, nil
}

// Adds new test databases for a template, intended to be run asynchronously from other operations in a separate goroutine, using the manager's WaitGroup to synchronize for shutdown.
func (m *Manager) addInitialTestDatabasesInBackground(template *templates.Template, count int) {

	ctx, cancel := context.WithCancel(context.Background())

	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		defer cancel()

		for i := 0; i < count; i++ {
			// TODO log error somewhere instead of silently swallowing it?
			_, _ = m.createTestDatabaseFromTemplate(ctx, template)
		}
	}()

	select {
	case <-m.closeChan:
		// manager was requested to stop
		cancel()
	case <-ctx.Done():
	}
}

func (m *Manager) makeTemplateDatabaseName(hash string) string {
	return fmt.Sprintf("%s_%s_%s", m.config.DatabasePrefix, m.config.TemplateDatabasePrefix, hash)
}

func (m *Manager) makeTestDatabasePrefix(hash string) string {
	return fmt.Sprintf("%s_%s_%s_", m.config.DatabasePrefix, m.config.TestDatabasePrefix, hash)
}
