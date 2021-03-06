// Copyright 2020 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package main provides a utility that bootstraps the initial database with
// users and realms.
package main

import (
	"context"
	"encoding/hex"
	"fmt"
	"math/rand"
	"os"
	"strconv"
	"time"

	firebase "firebase.google.com/go"
	firebaseauth "firebase.google.com/go/auth"
	"github.com/google/exposure-notifications-verification-server/pkg/api"
	"github.com/google/exposure-notifications-verification-server/pkg/config"
	"github.com/google/exposure-notifications-verification-server/pkg/database"
	"github.com/jinzhu/gorm"

	"github.com/google/exposure-notifications-server/pkg/logging"

	"github.com/sethvargo/go-envconfig"
	"github.com/sethvargo/go-signalcontext"
)

func main() {
	ctx, done := signalcontext.OnInterrupt()

	debug, _ := strconv.ParseBool(os.Getenv("LOG_DEBUG"))
	logger := logging.NewLogger(debug)
	ctx = logging.WithLogger(ctx, logger)

	err := realMain(ctx)
	done()

	if err != nil {
		logger.Fatal(err)
	}
}

func realMain(ctx context.Context) error {
	logger := logging.FromContext(ctx).Named("seed")

	// Database
	var dbConfig database.Config
	if err := config.ProcessWith(ctx, &dbConfig, envconfig.OsLookuper()); err != nil {
		return fmt.Errorf("failed to process config: %w", err)
	}

	db, err := dbConfig.Load(ctx)
	if err != nil {
		return fmt.Errorf("failed to load database config: %w", err)
	}
	if err := db.Open(ctx); err != nil {
		return fmt.Errorf("failed to connect to database: %w", err)
	}
	defer db.Close()

	// Firebase
	var fbConfig config.FirebaseConfig
	if err := config.ProcessWith(ctx, &fbConfig, envconfig.OsLookuper()); err != nil {
		return fmt.Errorf("failed to parse firebase config: %w", err)
	}

	fb, err := firebase.NewApp(ctx, &firebase.Config{
		DatabaseURL:   fbConfig.DatabaseURL,
		ProjectID:     fbConfig.ProjectID,
		StorageBucket: fbConfig.StorageBucket,
	})
	if err != nil {
		return fmt.Errorf("failed to setup firebase: %w", err)
	}
	firebaseAuth, err := fb.Auth(ctx)
	if err != nil {
		return fmt.Errorf("failed to configure firebase: %w", err)
	}

	// Create a realm
	realm1 := database.NewRealmWithDefaults("Narnia")
	realm1.RegionCode = "US-PA"
	realm1.AbusePreventionEnabled = true
	if err := db.SaveRealm(realm1, database.System); err != nil {
		return fmt.Errorf("failed to create realm: %w: %v", err, realm1.ErrorMessages())
	}
	logger.Infow("created realm", "realm", realm1)

	// Create another realm
	realm2 := database.NewRealmWithDefaults("Wonderland")
	realm2.AllowedTestTypes = database.TestTypeLikely | database.TestTypeConfirmed
	realm2.RegionCode = "US-WA"
	realm2.AbusePreventionEnabled = true
	if err := db.SaveRealm(realm2, database.System); err != nil {
		return fmt.Errorf("failed to create realm: %w: %v", err, realm2.ErrorMessages())
	}
	logger.Infow("created realm", "realm", realm2)

	// Create users
	user := &database.User{Email: "user@example.com", Name: "Demo User"}
	if _, err := db.FindUserByEmail(user.Email); database.IsNotFound(err) {
		user.AddRealm(realm1)
		user.AddRealm(realm2)
		if err := db.SaveUser(user, database.System); err != nil {
			return fmt.Errorf("failed to create user: %w: %v", err, user.ErrorMessages())
		}
		logger.Infow("created user", "user", user)
	}

	if err := createFirebaseUser(ctx, firebaseAuth, user); err != nil {
		return err
	}
	logger.Infow("enabled user", "user", user)

	unverified := &database.User{Email: "unverified@example.com", Name: "Unverified User"}
	if _, err := db.FindUserByEmail(unverified.Email); database.IsNotFound(err) {
		unverified.AddRealm(realm1)
		if err := db.SaveUser(unverified, database.System); err != nil {
			return fmt.Errorf("failed to create unverified: %w: %v", err, unverified.ErrorMessages())
		}
		logger.Infow("created user", "user", unverified)
	}

	admin := &database.User{Email: "admin@example.com", Name: "Admin User"}
	if _, err := db.FindUserByEmail(admin.Email); database.IsNotFound(err) {
		admin.AddRealm(realm1)
		admin.AddRealmAdmin(realm1)
		if err := db.SaveUser(admin, database.System); err != nil {
			return fmt.Errorf("failed to create admin: %w: %v", err, admin.ErrorMessages())
		}
		logger.Infow("created admin", "admin", admin)
	}

	if err := createFirebaseUser(ctx, firebaseAuth, admin); err != nil {
		return err
	}
	logger.Infow("enabled admin", "admin", admin)

	super := &database.User{Email: "super@example.com", Name: "Super User", SystemAdmin: true}
	if _, err := db.FindUserByEmail(super.Email); database.IsNotFound(err) {
		if err := db.SaveUser(super, database.System); err != nil {
			return fmt.Errorf("failed to create super: %w: %v", err, super.ErrorMessages())
		}
		logger.Infow("created super", "super", super)
	}

	if err := createFirebaseUser(ctx, firebaseAuth, super); err != nil {
		return err
	}
	logger.Infow("enabled super", "super", super)

	// Create a device API key
	deviceAPIKey, err := realm1.CreateAuthorizedApp(db, &database.AuthorizedApp{
		Name:       "Corona Capture",
		APIKeyType: database.APIKeyTypeDevice,
	}, admin)
	if err != nil {
		return fmt.Errorf("failed to create device api key: %w", err)
	}
	logger.Infow("created device api key", "key", deviceAPIKey)

	// Create some Apps
	apps := []*database.MobileApp{
		{
			Name:    "Example iOS app",
			RealmID: realm1.ID,
			URL:     "http://google.com/",
			OS:      database.OSTypeIOS,
			AppID:   "ios.example.app",
		},
		{
			Name:    "Example Android app",
			RealmID: realm1.ID,
			URL:     "http://google.com",
			OS:      database.OSTypeAndroid,
			AppID:   "android.example.app",
			SHA:     "AA:AA:AA:AA:AA:AA:AA:AA:AA:AA:AA:AA:AA:AA:AA:AA:AA:AA:AA:AA:AA:AA:AA:AA:AA:AA:AA:AA:AA:AA:AA:AA",
		},
	}
	for i := range apps {
		app := apps[i]
		if err := db.SaveMobileApp(app, database.System); err != nil {
			return fmt.Errorf("failed to create app: %w", err)
		}
	}

	// Create an admin API key
	adminAPIKey, err := realm1.CreateAuthorizedApp(db, &database.AuthorizedApp{
		Name:       "Tracing Tracker",
		APIKeyType: database.APIKeyTypeAdmin,
	}, admin)
	if err != nil {
		return fmt.Errorf("failed to create admin api key: %w", err)
	}
	logger.Infow("created device api key", "key", adminAPIKey)

	// Generate some codes
	now := time.Now().UTC()
	users := []*database.User{user, unverified, super, admin}
	externalIDs := make([]string, 4)
	for i := range externalIDs {
		b := make([]byte, 8)
		if _, err := rand.Read(b); err != nil {
			return fmt.Errorf("failed to read rand: %w", err)
		}
		externalIDs[i] = hex.EncodeToString(b)
	}

	for day := 1; day <= 30; day++ {
		max := rand.Intn(50)
		for i := 0; i < max; i++ {
			date := now.Add(time.Duration(day) * -24 * time.Hour)

			issuingUserID := uint(0)
			issuingAppID := uint(0)
			issuingExternalID := ""

			// Random determine if this was issued by an app (60% chance).
			if rand.Intn(10) <= 6 {
				issuingAppID = apps[rand.Intn(len(apps))].ID

				// Random determine if the code had an external audit.
				if rand.Intn(2) == 0 {
					b := make([]byte, 8)
					if _, err := rand.Read(b); err != nil {
						return fmt.Errorf("failed to read rand: %w", err)
					}
					issuingExternalID = externalIDs[rand.Intn(len(externalIDs))]
				}
			} else {
				issuingUserID = users[rand.Intn(len(users))].ID
			}

			code := fmt.Sprintf("%08d", rand.Intn(99999999))
			longCode := fmt.Sprintf("%015d", rand.Intn(999999999999999))
			testDate := now.Add(-48 * time.Hour)

			verificationCode := &database.VerificationCode{
				Model: gorm.Model{
					CreatedAt: date,
				},
				RealmID:       realm1.ID,
				Code:          code,
				ExpiresAt:     now.Add(15 * time.Minute),
				LongCode:      longCode,
				LongExpiresAt: now.Add(24 * time.Hour),
				TestType:      "confirmed",
				SymptomDate:   &testDate,
				TestDate:      &testDate,

				IssuingUserID:     issuingUserID,
				IssuingAppID:      issuingAppID,
				IssuingExternalID: issuingExternalID,
			}
			// If a verification code already exists, it will fail to save, and we retry.
			if err := db.SaveVerificationCode(verificationCode, 672*time.Hour); err != nil {
				return fmt.Errorf("failed to create verification code: %w", err)
			}

			// 40% chance that the code is claimed
			if rand.Intn(10) <= 4 {
				accept := map[string]struct{}{
					api.TestTypeConfirmed: {},
					api.TestTypeLikely:    {},
					api.TestTypeNegative:  {},
				}
				if _, err := db.VerifyCodeAndIssueToken(realm1.ID, code, accept, 24*time.Hour); err != nil {
					return fmt.Errorf("failed to claim token: %w", err)
				}
			}
		}
	}

	return nil
}

func createFirebaseUser(ctx context.Context, firebaseAuth *firebaseauth.Client, user *database.User) error {
	existing, err := firebaseAuth.GetUserByEmail(ctx, user.Email)
	if err != nil && !firebaseauth.IsUserNotFound(err) {
		return fmt.Errorf("failed to get user by email %v: %w", user.Email, err)
	}

	// User exists, verify email
	if existing != nil {
		// Already verified
		if existing.EmailVerified {
			return nil
		}

		update := (&firebaseauth.UserToUpdate{}).
			EmailVerified(true)

		if _, err := firebaseAuth.UpdateUser(ctx, existing.UID, update); err != nil {
			return fmt.Errorf("failed to update user %v: %w", user.Email, err)
		}

		return nil
	}

	// User does not exist
	create := (&firebaseauth.UserToCreate{}).
		Email(user.Email).
		EmailVerified(true).
		DisplayName(user.Name).
		Password("password")

	if _, err := firebaseAuth.CreateUser(ctx, create); err != nil {
		return fmt.Errorf("failed to create user %v: %w", user.Email, err)
	}

	return nil
}
