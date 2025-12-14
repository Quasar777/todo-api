package main

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

type Todo struct {
	ID        string    `json:"id" bson:"_id"`
	Title     string    `json:"title" bson:"title"`
	Completed bool      `json:"completed" bson:"completed"`
	CreatedAt time.Time `json:"createdAt" bson:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt" bson:"updatedAt"`
}

type Repo interface {
	List(ctx context.Context) ([]Todo, error)
	Create(ctx context.Context, title string) (Todo, error)
	Get(ctx context.Context, id string) (Todo, bool, error)
	Update(ctx context.Context, id string, title *string, completed *bool) (Todo, bool, error)
	Delete(ctx context.Context, id string) (bool, error)
}

// ---------------- InMemory ----------------

type InMemoryRepo struct {
	mu    sync.RWMutex
	items map[string]Todo
	next  int
}

func NewInMemoryRepo() *InMemoryRepo {
	return &InMemoryRepo{items: map[string]Todo{}, next: 1}
}

func (r *InMemoryRepo) List(_ context.Context) ([]Todo, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Todo, 0, len(r.items))
	for _, t := range r.items {
		out = append(out, t)
	}
	return out, nil
}

func (r *InMemoryRepo) Create(_ context.Context, title string) (Todo, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	id := itoa(r.next)
	r.next++
	now := time.Now().UTC()
	t := Todo{ID: id, Title: title, Completed: false, CreatedAt: now, UpdatedAt: now}
	r.items[id] = t
	return t, nil
}

func (r *InMemoryRepo) Get(_ context.Context, id string) (Todo, bool, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	t, ok := r.items[id]
	return t, ok, nil
}

func (r *InMemoryRepo) Update(_ context.Context, id string, title *string, completed *bool) (Todo, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	t, ok := r.items[id]
	if !ok {
		return Todo{}, false, nil
	}
	if title != nil {
		t.Title = *title
	}
	if completed != nil {
		t.Completed = *completed
	}
	t.UpdatedAt = time.Now().UTC()
	r.items[id] = t
	return t, true, nil
}

func (r *InMemoryRepo) Delete(_ context.Context, id string) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.items[id]; !ok {
		return false, nil
	}
	delete(r.items, id)
	return true, nil
}

// ---------------- Mongo ----------------

type MongoRepo struct {
	col *mongo.Collection
}

func NewMongoRepo(ctx context.Context, uri string) (*MongoRepo, error) {
	client, err := mongo.Connect(ctx, options.Client().ApplyURI(uri))
	if err != nil {
		return nil, err
	}
	if err := client.Ping(ctx, nil); err != nil {
		return nil, err
	}
	db := client.Database(getEnv("MONGO_DB", "todo"))
	col := db.Collection(getEnv("MONGO_COLLECTION", "todos"))
	return &MongoRepo{col: col}, nil
}

func (r *MongoRepo) List(ctx context.Context) ([]Todo, error) {
	cur, err := r.col.Find(ctx, bson.D{})
	if err != nil {
		return nil, err
	}
	defer cur.Close(ctx)

	var out []Todo
	for cur.Next(ctx) {
		var t Todo
		if err := cur.Decode(&t); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, cur.Err()
}

func (r *MongoRepo) Create(ctx context.Context, title string) (Todo, error) {
	id := newID()
	now := time.Now().UTC()
	t := Todo{ID: id, Title: title, Completed: false, CreatedAt: now, UpdatedAt: now}

	_, err := r.col.InsertOne(ctx, t)
	return t, err
}

func (r *MongoRepo) Get(ctx context.Context, id string) (Todo, bool, error) {
	var t Todo
	err := r.col.FindOne(ctx, bson.M{"_id": id}).Decode(&t)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return Todo{}, false, nil
	}
	return t, err == nil, err
}

func (r *MongoRepo) Update(ctx context.Context, id string, title *string, completed *bool) (Todo, bool, error) {
	set := bson.M{"updatedAt": time.Now().UTC()}
	if title != nil {
		set["title"] = *title
	}
	if completed != nil {
		set["completed"] = *completed
	}

	res := r.col.FindOneAndUpdate(
		ctx,
		bson.M{"_id": id},
		bson.M{"$set": set},
		options.FindOneAndUpdate().SetReturnDocument(options.After),
	)

	var t Todo
	err := res.Decode(&t)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return Todo{}, false, nil
	}
	return t, err == nil, err
}

func (r *MongoRepo) Delete(ctx context.Context, id string) (bool, error) {
	res, err := r.col.DeleteOne(ctx, bson.M{"_id": id})
	if err != nil {
		return false, err
	}
	return res.DeletedCount == 1, nil
}

// ---------------- HTTP ----------------

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	var repo Repo
	if uri := os.Getenv("MONGO_URI"); uri != "" {
		mr, err := NewMongoRepo(ctx, uri)
		if err != nil {
			log.Printf("mongo connect failed, fallback to memory: %v", err)
			repo = NewInMemoryRepo()
		} else {
			log.Printf("using mongo repo")
			repo = mr
		}
	} else {
		log.Printf("MONGO_URI not set, using in-memory repo")
		repo = NewInMemoryRepo()
	}

	r := chi.NewRouter()
	r.Use(middleware.RequestID, middleware.RealIP, middleware.Logger, middleware.Recoverer)

	r.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	})

	r.Route("/todos", func(r chi.Router) {
		r.Get("/", func(w http.ResponseWriter, req *http.Request) {
			ctx := req.Context()
			todos, err := repo.List(ctx)
			if err != nil {
				writeError(w, http.StatusInternalServerError, "db error")
				return
			}
			writeJSON(w, http.StatusOK, todos)
		})

		r.Post("/", func(w http.ResponseWriter, req *http.Request) {
			var body struct {
				Title string `json:"title"`
			}
			if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
				writeError(w, http.StatusBadRequest, "invalid JSON")
				return
			}
			title := strings.TrimSpace(body.Title)
			if title == "" {
				writeError(w, http.StatusBadRequest, "title is required")
				return
			}
			t, err := repo.Create(req.Context(), title)
			if err != nil {
				writeError(w, http.StatusInternalServerError, "db error")
				return
			}
			writeJSON(w, http.StatusCreated, t)
		})

		r.Get("/{id}", func(w http.ResponseWriter, req *http.Request) {
			id := chi.URLParam(req, "id")
			t, ok, err := repo.Get(req.Context(), id)
			if err != nil {
				writeError(w, http.StatusInternalServerError, "db error")
				return
			}
			if !ok {
				writeError(w, http.StatusNotFound, "todo not found")
				return
			}
			writeJSON(w, http.StatusOK, t)
		})

		r.Put("/{id}", func(w http.ResponseWriter, req *http.Request) {
			id := chi.URLParam(req, "id")
			var body struct {
				Title     *string `json:"title"`
				Completed *bool   `json:"completed"`
			}
			if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
				writeError(w, http.StatusBadRequest, "invalid JSON")
				return
			}
			if body.Title != nil {
				t := strings.TrimSpace(*body.Title)
				if t == "" {
					writeError(w, http.StatusBadRequest, "title cannot be empty")
					return
				}
				body.Title = &t
			}
			todo, ok, err := repo.Update(req.Context(), id, body.Title, body.Completed)
			if err != nil {
				writeError(w, http.StatusInternalServerError, "db error")
				return
			}
			if !ok {
				writeError(w, http.StatusNotFound, "todo not found")
				return
			}
			writeJSON(w, http.StatusOK, todo)
		})

		r.Delete("/{id}", func(w http.ResponseWriter, req *http.Request) {
			id := chi.URLParam(req, "id")
			ok, err := repo.Delete(req.Context(), id)
			if err != nil {
				writeError(w, http.StatusInternalServerError, "db error")
				return
			}
			if !ok {
				writeError(w, http.StatusNotFound, "todo not found")
				return
			}
			w.WriteHeader(http.StatusNoContent)
		})
	})

	log.Printf("listening on :8080")
	log.Fatal(http.ListenAndServe(":8080", r))
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func getEnv(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [32]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + (n % 10))
		n /= 10
	}
	return string(b[i:])
}

func newID() string {
	// простой уникальный id без внешних либ:
	// 20251214T123456.789Z-<nanosec>
	now := time.Now().UTC()
	return now.Format("20060102T150405.000Z") + "-" + itoa(int(now.Nanosecond()))
}
