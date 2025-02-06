package main

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"time"
)

// RequestPayload defines the JSON structure we expect in the request body.
type RequestPayload struct {
	Kubeconfig        string `json:"kubeconfig,omitempty"`            // Optional; if not provided, use in-cluster or ~/.kube/config
	Namespace         string `json:"namespace,omitempty"`             // Required
	PersistenceDiskGB int    `json:"persistence_disk_size,omitempty"` // WordPress disk size in GB
	DatabaseDiskGB    int    `json:"database_disk_size,omitempty"`    // Database disk size in GB
	DeploymentName    string `json:"deployment_name,omitempty"`       // Prefix for naming
}

// APIResponse defines the JSON structure we return upon success/failure.
type APIResponse struct {
	Success   bool     `json:"success"`
	Message   string   `json:"message"`
	Resources []string `json:"resources,omitempty"` // Summaries of created resources
}

func main() {
	log.Println("Starting WordPress deployment API service...")
	http.HandleFunc("/create-wordpress", handleCreateWordPress)

	// You can set the port using the PORT environment variable; default is 8080.
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	log.Printf("Listening on port %s", port)
	if err := http.ListenAndServe(":"+port, nil); err != nil {
		log.Fatalf("Failed to start server: %v", err)
	}
}

// handleCreateWordPress is our main handler for receiving JSON requests to deploy the stack.
func handleCreateWordPress(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		respondJSON(w, APIResponse{
			Success: false,
			Message: "Only POST is allowed",
		})
		return
	}

	// Parse JSON payload
	decoder := json.NewDecoder(r.Body)
	var payload RequestPayload
	if err := decoder.Decode(&payload); err != nil {
		log.Printf("[ERROR] Failed to decode request body: %v", err)
		w.WriteHeader(http.StatusBadRequest)
		respondJSON(w, APIResponse{
			Success: false,
			Message: "Invalid JSON payload",
		})
		return
	}

	// Basic validation
	if payload.Namespace == "" {
		w.WriteHeader(http.StatusBadRequest)
		respondJSON(w, APIResponse{
			Success: false,
			Message: "namespace is required",
		})
		return
	}
	if payload.DeploymentName == "" {
		w.WriteHeader(http.StatusBadRequest)
		respondJSON(w, APIResponse{
			Success: false,
			Message: "deployment_name is required",
		})
		return
	}
	if payload.PersistenceDiskGB <= 0 {
		payload.PersistenceDiskGB = 5 // default disk size for WordPress
	}
	if payload.DatabaseDiskGB <= 0 {
		payload.DatabaseDiskGB = 5 // default disk size for Database
	}

	// Log the start of the process
	log.Printf("[INFO] Received request to deploy WordPress: %+v", payload)

	// Prepare Kubernetes client
	log.Println("[INFO] Initializing Kubernetes client...")
	clientSet, err := InitKubeClient(payload.Kubeconfig)
	if err != nil {
		log.Printf("[ERROR] Failed to create Kubernetes client: %v", err)
		w.WriteHeader(http.StatusInternalServerError)
		respondJSON(w, APIResponse{
			Success: false,
			Message: "Could not initialize Kubernetes client",
		})
		return
	}

	ctx := context.Background()

	// 1. Ensure namespace exists (or create if not).
	log.Printf("[INFO] Ensuring namespace '%s' exists...", payload.Namespace)
	nsErr := ensureNamespace(ctx, clientSet, payload.Namespace)
	if nsErr != nil {
		log.Printf("[ERROR] Failed to ensure namespace: %v", nsErr)
		w.WriteHeader(http.StatusInternalServerError)
		respondJSON(w, APIResponse{
			Success: false,
			Message: nsErr.Error(),
		})
		return
	}

	// 2. Create hostPath-based PV and PVC for MySQL
	dbPVName, dbPVCName := payload.DeploymentName+"-db-pv", payload.DeploymentName+"-db-pvc"
	log.Printf("[INFO] Creating hostPath PV/PVC for MySQL: PV=%s, PVC=%s", dbPVName, dbPVCName)
	err = createPersistentVolume(ctx, clientSet, payload.Namespace, dbPVName,
		"/mnt/data/"+payload.Namespace+"/"+payload.DeploymentName+"_db",
		payload.DatabaseDiskGB)
	if err != nil {
		log.Printf("[ERROR] Failed to create MySQL PV: %v", err)
		w.WriteHeader(http.StatusInternalServerError)
		respondJSON(w, APIResponse{
			Success: false,
			Message: "Failed to create MySQL PV",
		})
		return
	}

	err = createPersistentVolumeClaim(ctx, clientSet, payload.Namespace, dbPVCName, dbPVName, payload.DatabaseDiskGB)
	if err != nil {
		log.Printf("[ERROR] Failed to create MySQL PVC: %v", err)
		w.WriteHeader(http.StatusInternalServerError)
		respondJSON(w, APIResponse{
			Success: false,
			Message: "Failed to create MySQL PVC",
		})
		return
	}

	// 3. Create hostPath-based PV and PVC for WordPress
	wpPVName, wpPVCName := payload.DeploymentName+"-wp-pv", payload.DeploymentName+"-wp-pvc"
	log.Printf("[INFO] Creating hostPath PV/PVC for WordPress: PV=%s, PVC=%s", wpPVName, wpPVCName)
	err = createPersistentVolume(ctx, clientSet, payload.Namespace, wpPVName,
		"/mnt/data/"+payload.Namespace+"/"+payload.DeploymentName+"_wordpress",
		payload.PersistenceDiskGB)
	if err != nil {
		log.Printf("[ERROR] Failed to create WordPress PV: %v", err)
		w.WriteHeader(http.StatusInternalServerError)
		respondJSON(w, APIResponse{
			Success: false,
			Message: "Failed to create WordPress PV",
		})
		return
	}

	err = createPersistentVolumeClaim(ctx, clientSet, payload.Namespace, wpPVCName, wpPVName, payload.PersistenceDiskGB)
	if err != nil {
		log.Printf("[ERROR] Failed to create WordPress PVC: %v", err)
		w.WriteHeader(http.StatusInternalServerError)
		respondJSON(w, APIResponse{
			Success: false,
			Message: "Failed to create WordPress PVC",
		})
		return
	}

	// 4. Create Secret with random credentials for MySQL root and wordpress user.
	dbSecretName := payload.DeploymentName + "-db-secret"
	log.Printf("[INFO] Creating combined MySQL & WordPress secret: %s", dbSecretName)

	err = createWPMySQLSecret(ctx, clientSet, payload.Namespace, dbSecretName, payload.DeploymentName)
	if err != nil {
		log.Printf("[ERROR] Failed to create MySQL/WordPress Secret: %v", err)
		w.WriteHeader(http.StatusInternalServerError)
		respondJSON(w, APIResponse{
			Success: false,
			Message: "Failed to create MySQL/WordPress Secret",
		})
		return
	}

	// 5. Deploy MySQL (Deployment + Service)
	dbDeploymentName := payload.DeploymentName + "-db"
	dbServiceName := payload.DeploymentName + "-db-svc"

	log.Printf("[INFO] Creating MySQL deployment: %s", dbDeploymentName)
	err = createMySQLDeployment(ctx, clientSet, payload.Namespace, dbDeploymentName, dbPVCName, dbSecretName)
	if err != nil {
		log.Printf("[ERROR] Failed to create MySQL deployment: %v", err)
		w.WriteHeader(http.StatusInternalServerError)
		respondJSON(w, APIResponse{
			Success: false,
			Message: "Failed to create MySQL deployment",
		})
		return
	}

	log.Printf("[INFO] Creating MySQL service: %s", dbServiceName)
	err = createMySQLService(ctx, clientSet, payload.Namespace, dbServiceName, dbDeploymentName)
	if err != nil {
		log.Printf("[ERROR] Failed to create MySQL service: %v", err)
		w.WriteHeader(http.StatusInternalServerError)
		respondJSON(w, APIResponse{
			Success: false,
			Message: "Failed to create MySQL service",
		})
		return
	}

	// 6. Wait for MySQL deployment to be ready
	log.Println("[INFO] Waiting for MySQL deployment to be ready...")
	err = waitForDeploymentReady(ctx, clientSet, payload.Namespace, dbDeploymentName, 120*time.Second)
	if err != nil {
		log.Printf("[ERROR] MySQL deployment not ready in time: %v", err)
		w.WriteHeader(http.StatusInternalServerError)
		respondJSON(w, APIResponse{
			Success: false,
			Message: "MySQL deployment failed to become ready",
		})
		return
	}
	log.Println("[INFO] MySQL deployment is running and ready.")

	// 7. Deploy WordPress (Deployment + Service), referencing the same secret
	wpDeploymentName := payload.DeploymentName + "-wp"
	wpServiceName := payload.DeploymentName + "-wp-svc"

	log.Printf("[INFO] Creating WordPress deployment: %s", wpDeploymentName)
	err = createWordPressDeployment(ctx, clientSet, payload.Namespace, wpDeploymentName, wpPVCName, dbSecretName, dbServiceName)
	if err != nil {
		log.Printf("[ERROR] Failed to create WordPress deployment: %v", err)
		w.WriteHeader(http.StatusInternalServerError)
		respondJSON(w, APIResponse{
			Success: false,
			Message: "Failed to create WordPress deployment",
		})
		return
	}

	log.Printf("[INFO] Creating WordPress service: %s", wpServiceName)
	err = createWordPressService(ctx, clientSet, payload.Namespace, wpServiceName, wpDeploymentName)
	if err != nil {
		log.Printf("[ERROR] Failed to create WordPress service: %v", err)
		w.WriteHeader(http.StatusInternalServerError)
		respondJSON(w, APIResponse{
			Success: false,
			Message: "Failed to create WordPress service",
		})
		return
	}

	// 8. Wait for WordPress deployment to be ready
	log.Println("[INFO] Waiting for WordPress deployment to be ready...")
	err = waitForDeploymentReady(ctx, clientSet, payload.Namespace, wpDeploymentName, 120*time.Second)
	if err != nil {
		log.Printf("[ERROR] WordPress deployment not ready in time: %v", err)
		w.WriteHeader(http.StatusInternalServerError)
		respondJSON(w, APIResponse{
			Success: false,
			Message: "WordPress deployment failed to become ready",
		})
		return
	}
	log.Println("[INFO] WordPress deployment is running and ready.")

	// 9. All done, build a summary
	resources := []string{
		"Namespace: " + payload.Namespace,
		"PV: " + dbPVName,
		"PVC: " + dbPVCName,
		"PV: " + wpPVName,
		"PVC: " + wpPVCName,
		"Secret: " + dbSecretName,
		"MySQL Deployment: " + dbDeploymentName,
		"MySQL Service: " + dbServiceName,
		"WordPress Deployment: " + wpDeploymentName,
		"WordPress Service: " + wpServiceName,
	}

	log.Printf("[INFO] Successfully created resources: %+v", resources)

	respondJSON(w, APIResponse{
		Success:   true,
		Message:   "WordPress + MySQL stack created successfully. Strong random credentials have been set for MySQL.",
		Resources: resources,
	})
}

// Utility to send JSON response
func respondJSON(w http.ResponseWriter, resp APIResponse) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}
