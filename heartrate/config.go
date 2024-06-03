package heartrate

import (
    "encoding/json"
    "io/ioutil"
    "os"
)

type Config struct {
    TargetDeviceName string `json:"TargetDeviceName"`
    TargetDeviceMAC  string `json:"TargetDeviceMAC"`
    ScanTimeout      int    `json:"ScanTimeout"`
}

func LoadConfig(filename string) (Config, error) {
    var config Config
    file, err := os.Open(filename)
    if err != nil {
        return config, wrapError(err, "opening config file")
    }
    defer file.Close()
    bytes, err := ioutil.ReadAll(file)
    if err != nil {
        return config, wrapError(err, "reading config file")
    }
    err = json.Unmarshal(bytes, &config)
    if err != nil {
        return config, wrapError(err, "unmarshalling config JSON")
    }
    return config, nil
}
