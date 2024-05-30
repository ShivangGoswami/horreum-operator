package controller

import (
	"fmt"
	"strings"

	hyperfoilv1alpha1 "github.com/Hyperfoil/horreum-operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

func postgresConfigMap(cr *hyperfoilv1alpha1.Horreum) *corev1.ConfigMap {
	keycloakDbName := withDefault(cr.Spec.Keycloak.Database.Name, "keycloak")
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      cr.Name + "-postgresql-start",
			Namespace: cr.Namespace,
			Labels: map[string]string{
				"app": cr.Name,
			},
		},
		Data: map[string]string{
			"init_keycloak.sh": `
				if psql -t -c "SELECT 1 FROM pg_roles WHERE rolname = '$KEYCLOAK_USER';" | grep -q 1; then
					echo "Database role $KEYCLOAK_USER already exists.";
				else
					psql -c "CREATE ROLE \"$KEYCLOAK_USER\" noinherit login password '$KEYCLOAK_PASSWORD';";
				fi
				if psql -t -c "SELECT 1 FROM pg_database WHERE datname = '` + keycloakDbName + `';" | grep -q 1; then
					echo "Database "` + keycloakDbName + `" already exists.";
				else
					psql -c "CREATE DATABASE ` + keycloakDbName + ` WITH OWNER = '$KEYCLOAK_USER';";
				fi
			`,
			"init_app.sh": `
			    # Adding extension pgcrypto requires superuser priviledges
				psql -c 'ALTER ROLE "'$POSTGRESQL_USER'" WITH SUPERUSER';
				if psql -t -c "SELECT 1 FROM pg_roles WHERE rolname = '$APP_USER';" | grep -q 1; then
					echo "Database role $APP_USER already exists.";
				else
					psql -c "CREATE ROLE \"$APP_USER\" noinherit login password '$APP_PASSWORD';"
				fi
			`,
		},
	}
}

func postgresPod(cr *hyperfoilv1alpha1.Horreum, r *HorreumReconciler) *corev1.Pod {
	labels := map[string]string{
		"app":     cr.Name,
		"service": "db",
	}

	dbVolumeSrc := corev1.VolumeSource{}
	if cr.Spec.Postgres.PersistentVolumeClaim != "" {
		dbVolumeSrc = corev1.VolumeSource{
			PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
				ClaimName: cr.Spec.Postgres.PersistentVolumeClaim,
			},
		}
	} else {
		dbVolumeSrc = corev1.VolumeSource{
			EmptyDir: &corev1.EmptyDirVolumeSource{},
		}
	}
	image := dbImage(cr, r.UseRedHatImages)
	envs := []corev1.EnvVar{
		secretEnv("KEYCLOAK_USER", keycloakDbSecret(cr), corev1.BasicAuthUsernameKey),
		secretEnv("KEYCLOAK_PASSWORD", keycloakDbSecret(cr), corev1.BasicAuthPasswordKey),
		secretEnv("APP_USER", appUserSecret(cr), corev1.BasicAuthUsernameKey),
		secretEnv("APP_PASSWORD", appUserSecret(cr), corev1.BasicAuthPasswordKey),
		secretEnv("APP_DB_SECRET", appUserSecret(cr), "dbsecret"),
	}

	var userId int64
	var initDir string
	if strings.HasPrefix(image, "docker.io/library/postgres") || strings.HasPrefix(image, "postgres") {
		userId = int64(999)
		initDir = "/docker-entrypoint-initdb.d/"
		envs = append(envs,
			corev1.EnvVar{
				Name:  "POSTGRES_DB",
				Value: withDefault(cr.Spec.Database.Name, "horreum"),
			},
			corev1.EnvVar{
				Name:  "PGDATABASE",
				Value: withDefault(cr.Spec.Database.Name, "horreum"),
			},
			corev1.EnvVar{
				Name:  "PGDATA",
				Value: "/var/lib/pgsql/data",
			},
			secretEnv("PGUSER", dbAdminSecret(cr), corev1.BasicAuthUsernameKey),
			secretEnv("POSTGRES_USER", dbAdminSecret(cr), corev1.BasicAuthUsernameKey),
			// because of the init script
			secretEnv("POSTGRESQL_USER", dbAdminSecret(cr), corev1.BasicAuthUsernameKey),
			secretEnv("POSTGRES_PASSWORD", dbAdminSecret(cr), corev1.BasicAuthPasswordKey),
		)
	} else { // Red Hat image
		userId = int64(26)
		initDir = "/opt/app-root/src/postgresql-start"
		envs = append(envs,
			corev1.EnvVar{
				Name:  "POSTGRESQL_DATABASE",
				Value: withDefault(cr.Spec.Database.Name, "horreum"),
			},
			secretEnv("POSTGRESQL_USER", dbAdminSecret(cr), corev1.BasicAuthUsernameKey),
			secretEnv("POSTGRESQL_PASSWORD", dbAdminSecret(cr), corev1.BasicAuthPasswordKey))
	}

	if cr.Spec.Postgres.User != nil {
		userId = *cr.Spec.Postgres.User
	}
	initContainers := []corev1.Container{}
	if !r.UseRedHatImages {
		initContainers = append(initContainers, corev1.Container{
			Name:    "init",
			Image:   image,
			Command: []string{"chown", fmt.Sprint(userId), "/var/lib/pgsql/data"},
			SecurityContext: &corev1.SecurityContext{
				RunAsUser: &[]int64{0}[0],
			},
			VolumeMounts: []corev1.VolumeMount{
				{
					Name:      "db-volume",
					MountPath: "/var/lib/pgsql/data",
				},
			},
		})
	}
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      cr.Name + "-db",
			Namespace: cr.Namespace,
			Labels:    labels,
		},
		Spec: corev1.PodSpec{
			InitContainers: initContainers,
			SecurityContext: &corev1.PodSecurityContext{
				FSGroup: &[]int64{userId}[0],
			},
			Containers: []corev1.Container{
				{
					Name:  "postgres",
					Image: image,
					Env:   envs,
					Ports: []corev1.ContainerPort{
						{
							Name:          "postgres",
							ContainerPort: 5432,
						},
					},
					SecurityContext: &corev1.SecurityContext{
						RunAsUser: &[]int64{userId}[0],
					},
					VolumeMounts: []corev1.VolumeMount{
						{
							Name:      "db-volume",
							MountPath: "/var/lib/pgsql/data",
						},
						{
							Name:      "postgresql-start",
							MountPath: initDir,
						},
					},
				},
			},
			Volumes: []corev1.Volume{
				{
					Name:         "db-volume",
					VolumeSource: dbVolumeSrc,
				},
				{
					Name: "postgresql-start",
					VolumeSource: corev1.VolumeSource{
						ConfigMap: &corev1.ConfigMapVolumeSource{
							LocalObjectReference: corev1.LocalObjectReference{
								Name: cr.Name + "-postgresql-start",
							},
						},
					},
				},
			},
		},
	}
}

func postgresService(cr *hyperfoilv1alpha1.Horreum) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      cr.Name + "-db",
			Namespace: cr.Namespace,
			Annotations: map[string]string{
				"service.beta.openshift.io/serving-cert-secret-name": cr.Name + "-postgres",
			},
		},
		Spec: corev1.ServiceSpec{
			Type: corev1.ServiceTypeClusterIP,
			Ports: []corev1.ServicePort{
				{
					Name: "postgres",
					Port: int32(5432),
					TargetPort: intstr.IntOrString{
						IntVal: 5432,
					},
				},
			},
			Selector: map[string]string{
				"app":     cr.Name,
				"service": "db",
			},
		},
	}
}
